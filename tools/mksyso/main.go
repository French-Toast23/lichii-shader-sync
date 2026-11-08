// mksyso writes a minimal amd64 COFF object (.syso) with Windows resources, so `go build` links them:
// the application manifest (Common Controls v6, which walk requires) and optionally an icon.
//
//	go run ./tools/mksyso gui/app.manifest [assets/icon.ico] gui/rsrc_windows_amd64.syso
//
// The icon becomes RT_GROUP_ICON id 1 (Explorer/taskbar icon; walk.NewIconFromResourceId(1)).
package main

import (
	"bytes"
	"encoding/binary"
	"io/ioutil"
	"log"
	"os"
	"sort"
)

const (
	rtIcon      = 3
	rtGroupIcon = 14
	rtManifest  = 24
	langEnUS    = 0x0409
)

type res struct {
	typ, id uint32
	data    []byte
}

func le(v ...interface{}) []byte {
	var b bytes.Buffer
	for _, x := range v {
		binary.Write(&b, binary.LittleEndian, x)
	}
	return b.Bytes()
}

// iconResources splits an .ico file into RT_ICON images plus one RT_GROUP_ICON directory.
func iconResources(ico []byte) []res {
	n := int(binary.LittleEndian.Uint16(ico[4:]))
	var out []res
	group := le(uint16(0), uint16(1), uint16(n))
	for i := 0; i < n; i++ {
		e := ico[6+16*i : 6+16*(i+1)]
		size := binary.LittleEndian.Uint32(e[8:])
		off := binary.LittleEndian.Uint32(e[12:])
		id := uint32(i + 1)
		out = append(out, res{rtIcon, id, ico[off : off+size]})
		group = append(group, e[:12]...) // w, h, colors, reserved, planes, bpp, bytes
		group = append(group, le(uint16(id))...)
	}
	return append(out, res{rtGroupIcon, 1, group})
}

func main() {
	if len(os.Args) != 3 && len(os.Args) != 4 {
		log.Fatal("usage: mksyso app.manifest [icon.ico] out.syso")
	}
	man, err := ioutil.ReadFile(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	rs := []res{{rtManifest, 1, man}}
	if len(os.Args) == 4 {
		ico, err := ioutil.ReadFile(os.Args[2])
		if err != nil {
			log.Fatal(err)
		}
		rs = append(rs, iconResources(ico)...)
	}
	outPath := os.Args[len(os.Args)-1]

	// group by type, ids ascending (resource directories must be sorted)
	byType := map[uint32][]res{}
	var types []uint32
	for _, r := range rs {
		if _, ok := byType[r.typ]; !ok {
			types = append(types, r.typ)
		}
		byType[r.typ] = append(byType[r.typ], r)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	for _, t := range types {
		l := byType[t]
		sort.Slice(l, func(i, j int) bool { return l[i].id < l[j].id })
	}

	dirSize := func(entries int) int { return 16 + 8*entries }
	// layout: root dir, per type: name dir, per resource: lang dir; then data entries; then data
	off := dirSize(len(types))
	typeDir := map[uint32]int{}
	langDir := map[[2]uint32]int{}
	for _, t := range types {
		typeDir[t] = off
		off += dirSize(len(byType[t]))
	}
	for _, t := range types {
		for _, r := range byType[t] {
			langDir[[2]uint32{t, r.id}] = off
			off += dirSize(1)
		}
	}
	dataEntry := map[[2]uint32]int{}
	for _, t := range types {
		for _, r := range byType[t] {
			dataEntry[[2]uint32{t, r.id}] = off
			off += 16
		}
	}
	dataAt := map[[2]uint32]int{}
	for _, t := range types {
		for _, r := range byType[t] {
			for off%8 != 0 {
				off++
			}
			dataAt[[2]uint32{t, r.id}] = off
			off += len(r.data)
		}
	}
	for off%8 != 0 {
		off++
	}

	sec := make([]byte, off)
	put := func(at int, b []byte) { copy(sec[at:], b) }
	dirHdr := func(n int) []byte { return le(uint32(0), uint32(0), uint16(0), uint16(0), uint16(0), uint16(n)) }
	put(0, dirHdr(len(types)))
	for i, t := range types {
		put(16+8*i, le(t, uint32(0x80000000|typeDir[t])))
		put(typeDir[t], dirHdr(len(byType[t])))
		for j, r := range byType[t] {
			k := [2]uint32{t, r.id}
			put(typeDir[t]+16+8*j, le(r.id, uint32(0x80000000|langDir[k])))
			put(langDir[k], dirHdr(1))
			put(langDir[k]+16, le(uint32(langEnUS), uint32(dataEntry[k])))
			put(dataEntry[k], le(uint32(dataAt[k]), uint32(len(r.data)), uint32(0), uint32(0)))
			put(dataAt[k], r.data)
		}
	}

	var relocs []int
	for _, t := range types {
		for _, r := range byType[t] {
			relocs = append(relocs, dataEntry[[2]uint32{t, r.id}])
		}
	}
	const hdr = 20 + 40
	relocOff := uint32(hdr + len(sec))
	symOff := relocOff + uint32(10*len(relocs))

	var out bytes.Buffer
	out.Write(le(uint16(0x8664), uint16(1), uint32(0), symOff, uint32(1), uint16(0), uint16(0)))
	out.WriteString(".rsrc\x00\x00\x00")
	out.Write(le(uint32(0), uint32(0), uint32(len(sec)), uint32(hdr), relocOff, uint32(0),
		uint16(len(relocs)), uint16(0), uint32(0x40000040)))
	out.Write(sec)
	for _, at := range relocs { // OffsetToData fields are image-relative (ADDR32NB) into .rsrc
		out.Write(le(uint32(at), uint32(0), uint16(3)))
	}
	out.WriteString(".rsrc\x00\x00\x00")
	out.Write(le(uint32(0), int16(1), uint16(0), uint8(3), uint8(0)))
	out.Write(le(uint32(4)))
	if err := ioutil.WriteFile(outPath, out.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
}
