package vmdk

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	sector      = 512
	grainSize   = 128
	grainBytes  = grainSize * sector
	gtEntries   = 512
	markerEOS   = 0
	markerGT    = 1
	markerGD    = 2
	markerFoot  = 3
	gdAtEnd     = ^uint64(0)
	headerFlags = 1 | 1<<16 | 1<<17
)

type writer struct {
	w        io.Writer
	pos      uint64
	capacity uint64
	gt       []uint32
}

func (w *writer) write(b []byte) error {
	if pad := len(b) % sector; pad != 0 {
		b = append(b, make([]byte, sector-pad)...)
	}
	_, err := w.w.Write(b)
	w.pos += uint64(len(b) / sector)
	return err
}

func header(capacity, descSectors, gdOffset uint64) []byte {
	h := make([]byte, sector)
	le := binary.LittleEndian
	copy(h[0:], "KDMV")
	le.PutUint32(h[4:], 3)
	le.PutUint32(h[8:], headerFlags)
	le.PutUint64(h[12:], capacity)
	le.PutUint64(h[20:], grainSize)
	le.PutUint64(h[28:], 1)
	le.PutUint64(h[36:], descSectors)
	le.PutUint32(h[44:], gtEntries)
	le.PutUint64(h[48:], 0)
	le.PutUint64(h[56:], gdOffset)
	le.PutUint64(h[64:], 1+descSectors)
	h[72] = 0
	h[73], h[74], h[75], h[76] = '\n', ' ', '\r', '\n'
	le.PutUint16(h[77:], 1)
	return h
}

func marker(lba uint64, size uint32, typ uint32) []byte {
	m := make([]byte, sector)
	binary.LittleEndian.PutUint64(m[0:], lba)
	binary.LittleEndian.PutUint32(m[8:], size)
	binary.LittleEndian.PutUint32(m[12:], typ)
	return m
}

func descriptor(capacity uint64, name string) []byte {
	cylinders := min(capacity/(16*63), 65535)
	d := fmt.Sprintf("# Disk DescriptorFile\nversion=1\nCID=fffffffe\nparentCID=ffffffff\ncreateType=\"streamOptimized\"\n\n# Extent description\nRW %d SPARSE \"%s\"\n\n# The Disk Data Base\n#DDB\n\nddb.adapterType = \"lsilogic\"\nddb.geometry.cylinders = \"%d\"\nddb.geometry.heads = \"16\"\nddb.geometry.sectors = \"63\"\nddb.virtualHWVersion = \"14\"\n", capacity, name, cylinders)
	b := []byte(d)
	return append(b, make([]byte, (sector-len(b)%sector)%sector)...)
}

func Write(w io.Writer, r io.Reader, sizeBytes int64, name string) error {
	capacity := (uint64(sizeBytes) + sector - 1) / sector
	desc := descriptor(capacity, name)
	vw := &writer{w: w, capacity: capacity}
	if err := vw.write(header(capacity, uint64(len(desc)/sector), gdAtEnd)); err != nil {
		return err
	}
	if err := vw.write(desc); err != nil {
		return err
	}
	grains := (capacity + grainSize - 1) / grainSize
	tables := (grains + gtEntries - 1) / gtEntries
	vw.gt = make([]uint32, tables*gtEntries)
	buf := make([]byte, grainBytes)
	var z bytes.Buffer
	for g := uint64(0); g < grains; g++ {
		n, err := io.ReadFull(r, buf)
		if err == io.EOF {
			break
		}
		if err != nil && err != io.ErrUnexpectedEOF {
			return err
		}
		clear(buf[n:])
		if allZero(buf) {
			continue
		}
		z.Reset()
		zw := zlib.NewWriter(&z)
		zw.Write(buf)
		zw.Close()
		vw.gt[g] = uint32(vw.pos)
		m := make([]byte, 12, 12+z.Len())
		binary.LittleEndian.PutUint64(m[0:], g*grainSize)
		binary.LittleEndian.PutUint32(m[8:], uint32(z.Len()))
		if err := vw.write(append(m, z.Bytes()...)); err != nil {
			return err
		}
		if n < grainBytes {
			break
		}
	}
	gd := make([]uint32, tables)
	for t := uint64(0); t < tables; t++ {
		if err := vw.write(marker(gtEntries*4/sector, 0, markerGT)); err != nil {
			return err
		}
		gd[t] = uint32(vw.pos)
		b := make([]byte, gtEntries*4)
		for i, v := range vw.gt[t*gtEntries : (t+1)*gtEntries] {
			binary.LittleEndian.PutUint32(b[i*4:], v)
		}
		if err := vw.write(b); err != nil {
			return err
		}
	}
	gdBytes := make([]byte, len(gd)*4)
	for i, v := range gd {
		binary.LittleEndian.PutUint32(gdBytes[i*4:], v)
	}
	if err := vw.write(marker((uint64(len(gdBytes))+sector-1)/sector, 0, markerGD)); err != nil {
		return err
	}
	gdOffset := vw.pos
	if err := vw.write(gdBytes); err != nil {
		return err
	}
	if err := vw.write(marker(1, 0, markerFoot)); err != nil {
		return err
	}
	if err := vw.write(header(capacity, uint64(len(desc)/sector), gdOffset)); err != nil {
		return err
	}
	return vw.write(marker(0, 0, markerEOS))
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
