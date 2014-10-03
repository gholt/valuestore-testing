package brimstore

import (
	"encoding/binary"
	"fmt"
	"github.com/gholt/brimutil"
	"github.com/spaolacci/murmur3"
	"io"
	"math"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"
)

var ErrKeyNotFound error = fmt.Errorf("key not found")

type ReadValue struct {
	KeyHashA uint64
	KeyHashB uint64
	Value    []byte
	Seq      uint64
	ReadChan chan error
	offset   uint32
}

type WriteValue struct {
	KeyHashA    uint64
	KeyHashB    uint64
	Value       []byte
	Seq         uint64
	WrittenChan chan error
}

type StoreOpts struct {
	Cores             int
	MaxValueSize      int
	MemTOCPageSize    int
	MemValuesPageSize int
	ChecksumInterval  int
}

func NewStoreOpts() *StoreOpts {
	opts := &StoreOpts{}
	if env := os.Getenv("BRIMSTORE_CORES"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			opts.Cores = val
		}
	}
	if opts.Cores <= 0 {
		opts.Cores = runtime.GOMAXPROCS(0)
	}
	if env := os.Getenv("BRIMSTORE_MAX_VALUE_SIZE"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			opts.MaxValueSize = val
		}
	}
	if opts.MaxValueSize <= 0 {
		opts.MaxValueSize = 4 * 1024 * 1024
	}
	if env := os.Getenv("BRIMSTORE_MEM_TOC_PAGE_SIZE"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			opts.MemTOCPageSize = val
		}
	}
	if opts.MemTOCPageSize <= 0 {
		opts.MemTOCPageSize = 1 << PowerOfTwoNeeded(uint64(opts.MaxValueSize+4))
	}
	if env := os.Getenv("BRIMSTORE_MEM_VALUES_PAGE_SIZE"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			opts.MemValuesPageSize = val
		}
	}
	if opts.MemValuesPageSize <= 0 {
		opts.MemValuesPageSize = 1 << PowerOfTwoNeeded(uint64(opts.MaxValueSize+4))
	}
	if env := os.Getenv("BRIMSTORE_CHECKSUM_INTERVAL"); env != "" {
		if val, err := strconv.Atoi(env); err == nil {
			opts.ChecksumInterval = val
		}
	}
	if opts.ChecksumInterval <= 0 {
		opts.ChecksumInterval = 65532
	}
	return opts
}

type Store struct {
	clearableMemBlockChan    chan *memBlock
	clearedMemBlockChan      chan *memBlock
	writeValueChans          []chan *WriteValue
	diskWritableMemBlockChan chan *memBlock
	freeTOCBlockChan         chan []byte
	pendingTOCBlockChan      chan []byte
	memWriterDoneChans       []chan struct{}
	memClearerDoneChans      []chan struct{}
	diskWriterDoneChan       chan struct{}
	tocWriterDoneChan        chan struct{}
	keyLocationBlocks        []keyLocationBlock
	keyLocationBlocksIDer    int32
	keyLocationMap           *keyLocationMap
	cores                    int
	maxValueSize             int
	memTOCPageSize           int
	memValuesPageSize        int
	checksumInterval         int
	diskWriterBytes          uint64
	tocWriterBytes           uint64
}

func NewStore(opts *StoreOpts) *Store {
	if opts == nil {
		opts = NewStoreOpts()
	}
	cores := opts.Cores
	if cores < 1 {
		cores = 1
	}
	maxValueSize := opts.MaxValueSize
	if maxValueSize < 0 {
		maxValueSize = 0
	}
	memTOCPageSize := opts.MemTOCPageSize
	if memTOCPageSize < 4096 {
		memTOCPageSize = 4096
	}
	memValuesPageSize := opts.MemValuesPageSize
	if memValuesPageSize < 4096 {
		memValuesPageSize = 4096
	}
	checksumInterval := opts.ChecksumInterval
	if checksumInterval < 1024 {
		checksumInterval = 1024
	} else if checksumInterval >= 4294967296 {
		checksumInterval = 4294967295
	}
	s := &Store{
		keyLocationBlocks:     make([]keyLocationBlock, 65536),
		keyLocationBlocksIDer: KEY_LOCATION_BLOCK_ID_OFFSET - 1,
		keyLocationMap:        newKeyLocationMap(),
		cores:                 cores,
		maxValueSize:          maxValueSize,
		memTOCPageSize:        memTOCPageSize,
		memValuesPageSize:     memValuesPageSize,
		checksumInterval:      checksumInterval,
	}
	return s
}

func (s *Store) MaxValueSize() int {
	return s.maxValueSize
}

func (s *Store) NewReadValue() *ReadValue {
	return &ReadValue{
		Value:    make([]byte, s.maxValueSize),
		ReadChan: make(chan error, 1),
	}
}

func (s *Store) Start() {
	s.clearableMemBlockChan = make(chan *memBlock, s.cores)
	s.clearedMemBlockChan = make(chan *memBlock, s.cores)
	s.writeValueChans = make([]chan *WriteValue, s.cores)
	s.diskWritableMemBlockChan = make(chan *memBlock, s.cores)
	s.freeTOCBlockChan = make(chan []byte, s.cores)
	s.pendingTOCBlockChan = make(chan []byte, s.cores)
	s.memWriterDoneChans = make([]chan struct{}, s.cores)
	s.memClearerDoneChans = make([]chan struct{}, s.cores)
	s.diskWriterDoneChan = make(chan struct{}, 1)
	s.tocWriterDoneChan = make(chan struct{}, 1)
	for i := 0; i < cap(s.clearableMemBlockChan); i++ {
		mb := &memBlock{
			toc:  make([]byte, 0, s.memTOCPageSize),
			data: make([]byte, 0, s.memValuesPageSize),
		}
		mb.id = s.addKeyLocationBlock(mb)
		s.clearableMemBlockChan <- mb
	}
	for i := 0; i < cap(s.clearedMemBlockChan); i++ {
		mb := &memBlock{
			toc:  make([]byte, 0, s.memTOCPageSize),
			data: make([]byte, s.memValuesPageSize),
		}
		mb.id = s.addKeyLocationBlock(mb)
		s.clearedMemBlockChan <- mb
	}
	for i := 0; i < len(s.writeValueChans); i++ {
		s.writeValueChans[i] = make(chan *WriteValue, s.cores)
	}
	for i := 0; i < cap(s.freeTOCBlockChan); i++ {
		s.freeTOCBlockChan <- make([]byte, 0, s.memTOCPageSize)
	}
	for i := 0; i < len(s.memWriterDoneChans); i++ {
		s.memWriterDoneChans[i] = make(chan struct{}, 1)
	}
	for i := 0; i < len(s.memClearerDoneChans); i++ {
		s.memClearerDoneChans[i] = make(chan struct{}, 1)
	}
	go s.tocWriter()
	go s.diskWriter()
	for i := 0; i < len(s.memClearerDoneChans); i++ {
		go s.memClearer(s.memClearerDoneChans[i])
	}
	for i := 0; i < len(s.writeValueChans); i++ {
		go s.memWriter(s.writeValueChans[i], s.memWriterDoneChans[i])
	}
}

func (s *Store) Stop() uint64 {
	for _, c := range s.writeValueChans {
		close(c)
	}
	for _, c := range s.memWriterDoneChans {
		<-c
	}
	close(s.diskWritableMemBlockChan)
	<-s.diskWriterDoneChan
	close(s.clearableMemBlockChan)
	for c := 0; c < cap(s.clearedMemBlockChan); c++ {
		<-s.clearedMemBlockChan
	}
	for c := 0; c < cap(s.clearableMemBlockChan); c++ {
		<-s.clearedMemBlockChan
	}
	close(s.clearedMemBlockChan)
	for _, c := range s.memClearerDoneChans {
		<-c
	}
	close(s.pendingTOCBlockChan)
	<-s.tocWriterDoneChan
	for s.keyLocationMap.isResizing() {
		time.Sleep(10 * time.Millisecond)
	}
	return s.diskWriterBytes + s.tocWriterBytes
}

func (s *Store) Get(r *ReadValue) {
	var id uint16
	id, r.offset, r.Seq = s.keyLocationMap.get(r.KeyHashA, r.KeyHashB)
	if id < KEY_LOCATION_BLOCK_ID_OFFSET {
		r.ReadChan <- ErrKeyNotFound
	}
	s.keyLocationBlock(id).Get(r)
}
func (s *Store) Put(w *WriteValue) {
	s.writeValueChans[int(w.KeyHashA>>1)%len(s.writeValueChans)] <- w
}

func (s *Store) keyLocationBlock(keyLocationBlockID uint16) keyLocationBlock {
	return s.keyLocationBlocks[keyLocationBlockID]
}

func (s *Store) addKeyLocationBlock(block keyLocationBlock) uint16 {
	id := atomic.AddInt32(&s.keyLocationBlocksIDer, 1)
	if id >= 65536 {
		panic("too many keyLocationBlocks")
	}
	s.keyLocationBlocks[id] = block
	return uint16(id)
}

func (s *Store) memClearer(memClearerDoneChan chan struct{}) {
	var tb []byte
	var tbTimestamp int64
	var tbOffset int
	for {
		mb := <-s.clearableMemBlockChan
		if mb == nil {
			if tb != nil {
				binary.LittleEndian.PutUint32(tb, uint32(len(tb)-4))
				s.pendingTOCBlockChan <- tb
			}
			break
		}
		if tb != nil && tbTimestamp != s.keyLocationBlock(mb.diskID).Timestamp() {
			binary.LittleEndian.PutUint32(tb, uint32(len(tb)-4))
			s.pendingTOCBlockChan <- tb
			tb = nil
		}
		for mbTOCOffset := 0; mbTOCOffset < len(mb.toc); mbTOCOffset += 28 {
			mbDataOffset := binary.LittleEndian.Uint32(mb.toc[mbTOCOffset:])
			a := binary.LittleEndian.Uint64(mb.toc[mbTOCOffset+4:])
			b := binary.LittleEndian.Uint64(mb.toc[mbTOCOffset+12:])
			q := binary.LittleEndian.Uint64(mb.toc[mbTOCOffset+20:])
			s.keyLocationMap.set(mb.diskID, mb.diskOffset+mbDataOffset, a, b, q)
			if tb != nil && tbOffset+28 > cap(tb) {
				binary.LittleEndian.PutUint32(tb, uint32(len(tb)-4))
				s.pendingTOCBlockChan <- tb
				tb = nil
			}
			if tb == nil {
				tb = <-s.freeTOCBlockChan
				tbTimestamp = s.keyLocationBlock(mb.diskID).Timestamp()
				tb = tb[:12]
				binary.LittleEndian.PutUint64(tb[4:], uint64(tbTimestamp))
				tbOffset = 12
			}
			tb = tb[:tbOffset+28]
			binary.LittleEndian.PutUint32(tb[tbOffset:], mb.diskOffset+uint32(mbDataOffset))
			binary.LittleEndian.PutUint64(tb[tbOffset+4:], a)
			binary.LittleEndian.PutUint64(tb[tbOffset+12:], b)
			binary.LittleEndian.PutUint64(tb[tbOffset+20:], q)
			tbOffset += 28
		}
		mb.diskID = 0
		mb.diskOffset = 0
		mb.toc = mb.toc[:0]
		mb.data = mb.data[:0]
		s.clearedMemBlockChan <- mb
	}
	memClearerDoneChan <- struct{}{}
}

func (s *Store) memWriter(writeValueChan chan *WriteValue, memWriterDoneChan chan struct{}) {
	var mb *memBlock
	var mbTOCOffset int
	var mbDataOffset int
	for {
		w := <-writeValueChan
		if w == nil {
			if mb != nil && len(mb.toc) > 0 {
				s.diskWritableMemBlockChan <- mb
			}
			break
		}
		vz := len(w.Value)
		if vz > s.maxValueSize {
			w.WrittenChan <- fmt.Errorf("value length of %d > %d", vz, s.maxValueSize)
			continue
		}
		if mb != nil && (mbTOCOffset+28 > cap(mb.toc) || mbDataOffset+4+vz > cap(mb.data)) {
			s.diskWritableMemBlockChan <- mb
			mb = nil
		}
		if mb == nil {
			mb = <-s.clearedMemBlockChan
			mbTOCOffset = 0
			mbDataOffset = 0
		}
		mb.toc = mb.toc[:mbTOCOffset+28]
		binary.LittleEndian.PutUint32(mb.toc[mbTOCOffset:], uint32(mbDataOffset))
		binary.LittleEndian.PutUint64(mb.toc[mbTOCOffset+4:], w.KeyHashA)
		binary.LittleEndian.PutUint64(mb.toc[mbTOCOffset+12:], w.KeyHashB)
		binary.LittleEndian.PutUint64(mb.toc[mbTOCOffset+20:], w.Seq)
		mbTOCOffset += 28
		mb.data = mb.data[:mbDataOffset+4+vz]
		binary.LittleEndian.PutUint32(mb.data[mbDataOffset:], uint32(vz))
		copy(mb.data[mbDataOffset+4:], w.Value)
		mbDataOffset += 4 + vz
		s.keyLocationMap.set(mb.id, uint32(mbDataOffset), w.KeyHashA, w.KeyHashB, w.Seq)
		w.WrittenChan <- nil
	}
	memWriterDoneChan <- struct{}{}
}

func (s *Store) diskWriter() {
	var db *diskBlock
	var dbOffset uint32
	head := []byte("BRIMSTORE VALUES v0             ")
	term := make([]byte, 16)
	copy(term[12:], "TERM")
	for {
		mb := <-s.diskWritableMemBlockChan
		if mb == nil {
			if db != nil {
				binary.LittleEndian.PutUint64(term[4:], uint64(dbOffset))
				if _, err := db.writer.Write(term); err != nil {
					panic(err)
				}
				if err := db.writer.Close(); err != nil {
					panic(err)
				}
				db.writer = nil
				dbOffset += 16
				s.diskWriterBytes += uint64(dbOffset) + uint64(dbOffset)/uint64(s.checksumInterval)*4
				if dbOffset%uint32(s.checksumInterval) != 0 {
					s.diskWriterBytes += 4
				}
			}
			break
		}
		// Use overflow to detect when to open a new disk file.
		// 48 is head and term usage
		if db != nil && dbOffset+uint32(len(mb.data))+48 < dbOffset {
			binary.LittleEndian.PutUint32(term[4:], dbOffset)
			if _, err := db.writer.Write(term); err != nil {
				panic(err)
			}
			if err := db.writer.Close(); err != nil {
				panic(err)
			}
			db.writer = nil
			dbOffset += 16
			s.diskWriterBytes += uint64(dbOffset) + uint64(dbOffset)/uint64(s.checksumInterval)*4
			if dbOffset%uint32(s.checksumInterval) != 0 {
				s.diskWriterBytes += 4
			}
			db = nil
		}
		if db == nil {
			db = &diskBlock{timestamp: time.Now().UnixNano()}
			name := fmt.Sprintf("%d.values", db.timestamp)
			fp, err := os.Create(name)
			if err != nil {
				panic(err)
			}
			db.writer = brimutil.NewMultiCoreChecksummedWriter(fp, s.checksumInterval, murmur3.New32, s.cores)
			db.readValueChans = make([]chan *ReadValue, 4)
			for i := 0; i < len(db.readValueChans); i++ {
				fp, err = os.Open(name)
				if err != nil {
					panic(err)
				}
				db.readValueChans[i] = make(chan *ReadValue, s.cores)
				go reader(brimutil.NewChecksummedReader(fp, s.checksumInterval, murmur3.New32), db.readValueChans[i])
			}
			db.id = s.addKeyLocationBlock(db)
			if _, err := db.writer.Write(head); err != nil {
				panic(err)
			}
			dbOffset = 32
		}
		if _, err := db.writer.Write(mb.data); err != nil {
			panic(err)
		}
		mb.diskID = db.id
		mb.diskOffset = dbOffset
		dbOffset += uint32(len(mb.data))
		s.clearableMemBlockChan <- mb
	}
	s.diskWriterDoneChan <- struct{}{}
}

func (s *Store) tocWriter() {
	var timestampA uint64
	var writerA io.WriteCloser
	var offsetA uint64
	var timestampB uint64
	var writerB io.WriteCloser
	var offsetB uint64
	head := []byte("BRIMSTORE TOC v0                ")
	term := make([]byte, 16)
	copy(term[12:], "TERM")
	for {
		t := <-s.pendingTOCBlockChan
		if t == nil {
			if writerB != nil {
				binary.LittleEndian.PutUint64(term[4:], offsetB)
				if _, err := writerB.Write(term); err != nil {
					panic(err)
				}
				if err := writerB.Close(); err != nil {
					panic(err)
				}
				offsetB += 16
				s.tocWriterBytes += offsetB + offsetB/uint64(s.checksumInterval*4)
				if offsetB%uint64(s.checksumInterval) != 0 {
					s.tocWriterBytes += 4
				}
			}
			if writerA != nil {
				binary.LittleEndian.PutUint64(term[4:], offsetA)
				if _, err := writerA.Write(term); err != nil {
					panic(err)
				}
				if err := writerA.Close(); err != nil {
					panic(err)
				}
				offsetA += 16
				s.tocWriterBytes += offsetA + offsetA/uint64(s.checksumInterval)*4
				if offsetA%uint64(s.checksumInterval) != 0 {
					s.tocWriterBytes += 4
				}
			}
			break
		}
		timestamp := binary.LittleEndian.Uint64(t[4:])
		switch timestamp {
		case timestampA:
			if _, err := writerA.Write(t); err != nil {
				panic(err)
			}
			offsetA += uint64(len(t))
		case timestampB:
			if _, err := writerB.Write(t); err != nil {
				panic(err)
			}
			offsetB += uint64(len(t))
		default:
			// An assumption is made here: If the timestamp for this toc block
			// doesn't match the last two seen timestamps then we expect no
			// more toc blocks for the oldest timestamp and can close that toc
			// file.
			if writerB != nil {
				binary.LittleEndian.PutUint64(term[4:], offsetB)
				if _, err := writerB.Write(term); err != nil {
					panic(err)
				}
				if err := writerB.Close(); err != nil {
					panic(err)
				}
				offsetB += 16
				s.tocWriterBytes += offsetB + offsetB/uint64(s.checksumInterval)*4
				if offsetB%uint64(s.checksumInterval) != 0 {
					s.tocWriterBytes += 4
				}
			}
			timestampB = timestampA
			writerB = writerA
			offsetB = offsetA
			timestampA = timestamp
			fp, err := os.Create(fmt.Sprintf("%d.toc", timestamp))
			if err != nil {
				panic(err)
			}
			//fp := &brimutil.NullIO{}
			writerA = brimutil.NewMultiCoreChecksummedWriter(fp, s.checksumInterval, murmur3.New32, s.cores)
			if _, err := writerA.Write(head); err != nil {
				panic(err)
			}
			if _, err := writerA.Write(t); err != nil {
				panic(err)
			}
			offsetA = 32 + uint64(len(t))
		}
		s.freeTOCBlockChan <- t[:0]
	}
	s.tocWriterDoneChan <- struct{}{}
}

type keyLocationBlock interface {
	Timestamp() int64
	Get(r *ReadValue)
}

type memBlock struct {
	id         uint16
	diskID     uint16
	diskOffset uint32
	toc        []byte
	data       []byte
}

func (m *memBlock) Timestamp() int64 {
	return math.MaxInt64
}

func (m *memBlock) Get(r *ReadValue) {
	z := binary.LittleEndian.Uint32(m.data[r.offset:])
	r.Value = r.Value[:z]
	copy(r.Value, m.data[r.offset+4:])
	r.ReadChan <- nil
}

type diskBlock struct {
	id             uint16
	timestamp      int64
	writer         io.WriteCloser
	readValueChans []chan *ReadValue
}

func (d *diskBlock) Timestamp() int64 {
	return d.timestamp
}

func (d *diskBlock) Get(r *ReadValue) {
	d.readValueChans[int(r.KeyHashA>>1)%len(d.readValueChans)] <- r
}

func reader(cr brimutil.ChecksummedReader, c chan *ReadValue) {
	zb := make([]byte, 4)
	for {
		r := <-c
		cr.Seek(int64(r.offset), 0)
		if _, err := io.ReadFull(cr, zb); err != nil {
			r.ReadChan <- err
		}
		z := binary.LittleEndian.Uint32(zb)
		r.Value = r.Value[:z]
		if _, err := io.ReadFull(cr, r.Value); err != nil {
			r.ReadChan <- err
		}
		r.ReadChan <- nil
	}
}
