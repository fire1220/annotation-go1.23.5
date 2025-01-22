// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"bytes"
	"compress/zlib"
	"debug/macho"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"reflect"
	"unsafe"
)

type loadCmd struct {
	Cmd macho.LoadCmd
	Len uint32
}

type dyldInfoCmd struct {
	Cmd                      macho.LoadCmd
	Len                      uint32
	RebaseOff, RebaseLen     uint32
	BindOff, BindLen         uint32
	WeakBindOff, WeakBindLen uint32
	LazyBindOff, LazyBindLen uint32
	ExportOff, ExportLen     uint32
}

type linkEditDataCmd struct {
	Cmd              macho.LoadCmd
	Len              uint32
	DataOff, DataLen uint32
}

type encryptionInfoCmd struct {
	Cmd                macho.LoadCmd
	Len                uint32
	CryptOff, CryptLen uint32
	CryptId            uint32
}

type uuidCmd struct {
	Cmd  macho.LoadCmd
	Len  uint32
	Uuid [16]byte
}

type loadCmdReader struct {
	offset, next int64
	f            *os.File
	order        binary.ByteOrder
}

func (r *loadCmdReader) Next() (loadCmd, error) {
	var cmd loadCmd

	r.offset = r.next
	if _, err := r.f.Seek(r.offset, 0); err != nil {
		return cmd, err
	}
	if err := binary.Read(r.f, r.order, &cmd); err != nil {
		return cmd, err
	}
	r.next = r.offset + int64(cmd.Len)
	return cmd, nil
}

func (r loadCmdReader) ReadAt(offset int64, data interface{}) error {
	if _, err := r.f.Seek(r.offset+offset, 0); err != nil {
		return err
	}
	return binary.Read(r.f, r.order, data)
}

func (r loadCmdReader) WriteAt(offset int64, data interface{}) error {
	if _, err := r.f.Seek(r.offset+offset, 0); err != nil {
		return err
	}
	return binary.Write(r.f, r.order, data)
}

// machoCombineDwarf merges dwarf info generated by dsymutil into a macho executable.
//
// With internal linking, DWARF is embedded into the executable, this lets us do the
// same for external linking.
// exef is the file of the executable with no DWARF. It must have enough room in the macho
// header to add the DWARF sections. (Use ld's -headerpad option)
// exem is the macho representation of exef.
// dsym is the path to the macho file containing DWARF from dsymutil.
// outexe is the path where the combined executable should be saved.
func machoCombineDwarf(ctxt *Link, exef *os.File, exem *macho.File, dsym, outexe string) error {
	dwarff, err := os.Open(dsym)
	if err != nil {
		return err
	}
	defer dwarff.Close()
	outf, err := os.OpenFile(outexe, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer outf.Close()
	dwarfm, err := macho.NewFile(dwarff)
	if err != nil {
		return err
	}
	defer dwarfm.Close()

	// The string table needs to be the last thing in the file
	// for code signing to work. So we'll need to move the
	// linkedit section, but all the others can be copied directly.
	linkseg := exem.Segment("__LINKEDIT")
	if linkseg == nil {
		return fmt.Errorf("missing __LINKEDIT segment")
	}

	if _, err := exef.Seek(0, 0); err != nil {
		return err
	}
	if _, err := io.CopyN(outf, exef, int64(linkseg.Offset)); err != nil {
		return err
	}

	realdwarf := dwarfm.Segment("__DWARF")
	if realdwarf == nil {
		return fmt.Errorf("missing __DWARF segment")
	}

	// Try to compress the DWARF sections. This includes some Apple
	// proprietary sections like __apple_types.
	compressedSects, compressedBytes, err := machoCompressSections(ctxt, dwarfm)
	if err != nil {
		return err
	}

	// Now copy the dwarf data into the output.
	// Kernel requires all loaded segments to be page-aligned in the file,
	// even though we mark this one as being 0 bytes of virtual address space.
	dwarfstart := Rnd(int64(linkseg.Offset), *FlagRound)
	if _, err := outf.Seek(dwarfstart, 0); err != nil {
		return err
	}

	if _, err := dwarff.Seek(int64(realdwarf.Offset), 0); err != nil {
		return err
	}

	// Write out the compressed sections, or the originals if we gave up
	// on compressing them.
	var dwarfsize uint64
	if compressedBytes != nil {
		dwarfsize = uint64(len(compressedBytes))
		if _, err := outf.Write(compressedBytes); err != nil {
			return err
		}
	} else {
		if _, err := io.CopyN(outf, dwarff, int64(realdwarf.Filesz)); err != nil {
			return err
		}
		dwarfsize = realdwarf.Filesz
	}

	// And finally the linkedit section.
	if _, err := exef.Seek(int64(linkseg.Offset), 0); err != nil {
		return err
	}
	linkstart := Rnd(dwarfstart+int64(dwarfsize), *FlagRound)
	if _, err := outf.Seek(linkstart, 0); err != nil {
		return err
	}
	if _, err := io.Copy(outf, exef); err != nil {
		return err
	}

	// Now we need to update the headers.
	textsect := exem.Section("__text")
	if textsect == nil {
		return fmt.Errorf("missing __text section")
	}

	cmdOffset := unsafe.Sizeof(exem.FileHeader)
	if is64bit := exem.Magic == macho.Magic64; is64bit {
		// mach_header_64 has one extra uint32.
		cmdOffset += unsafe.Sizeof(exem.Magic)
	}
	dwarfCmdOffset := uint32(cmdOffset) + exem.FileHeader.Cmdsz
	availablePadding := textsect.Offset - dwarfCmdOffset
	if availablePadding < realdwarf.Len {
		return fmt.Errorf("no room to add dwarf info. Need at least %d padding bytes, found %d", realdwarf.Len, availablePadding)
	}
	// First, copy the dwarf load command into the header. It will be
	// updated later with new offsets and lengths as necessary.
	if _, err := outf.Seek(int64(dwarfCmdOffset), 0); err != nil {
		return err
	}
	if _, err := io.CopyN(outf, bytes.NewReader(realdwarf.Raw()), int64(realdwarf.Len)); err != nil {
		return err
	}
	if _, err := outf.Seek(int64(unsafe.Offsetof(exem.FileHeader.Ncmd)), 0); err != nil {
		return err
	}
	if err := binary.Write(outf, exem.ByteOrder, exem.Ncmd+1); err != nil {
		return err
	}
	if err := binary.Write(outf, exem.ByteOrder, exem.Cmdsz+realdwarf.Len); err != nil {
		return err
	}

	reader := loadCmdReader{next: int64(cmdOffset), f: outf, order: exem.ByteOrder}
	for i := uint32(0); i < exem.Ncmd; i++ {
		cmd, err := reader.Next()
		if err != nil {
			return err
		}
		linkoffset := uint64(linkstart) - linkseg.Offset
		switch cmd.Cmd {
		case macho.LoadCmdSegment64:
			err = machoUpdateSegment(reader, linkseg, linkoffset)
		case macho.LoadCmdSegment:
			panic("unexpected 32-bit segment")
		case LC_DYLD_INFO, LC_DYLD_INFO_ONLY:
			err = machoUpdateLoadCommand(reader, linkseg, linkoffset, &dyldInfoCmd{}, "RebaseOff", "BindOff", "WeakBindOff", "LazyBindOff", "ExportOff")
		case macho.LoadCmdSymtab:
			err = machoUpdateLoadCommand(reader, linkseg, linkoffset, &macho.SymtabCmd{}, "Symoff", "Stroff")
		case macho.LoadCmdDysymtab:
			err = machoUpdateLoadCommand(reader, linkseg, linkoffset, &macho.DysymtabCmd{}, "Tocoffset", "Modtaboff", "Extrefsymoff", "Indirectsymoff", "Extreloff", "Locreloff")
		case LC_CODE_SIGNATURE, LC_SEGMENT_SPLIT_INFO, LC_FUNCTION_STARTS, LC_DATA_IN_CODE, LC_DYLIB_CODE_SIGN_DRS,
			LC_DYLD_EXPORTS_TRIE, LC_DYLD_CHAINED_FIXUPS:
			err = machoUpdateLoadCommand(reader, linkseg, linkoffset, &linkEditDataCmd{}, "DataOff")
		case LC_ENCRYPTION_INFO, LC_ENCRYPTION_INFO_64:
			err = machoUpdateLoadCommand(reader, linkseg, linkoffset, &encryptionInfoCmd{}, "CryptOff")
		case LC_UUID:
			var u uuidCmd
			err = reader.ReadAt(0, &u)
			if err == nil {
				copy(u.Uuid[:], uuidFromGoBuildId(*flagBuildid))
				err = reader.WriteAt(0, &u)
			}
		case macho.LoadCmdDylib, macho.LoadCmdThread, macho.LoadCmdUnixThread,
			LC_PREBOUND_DYLIB, LC_VERSION_MIN_MACOSX, LC_VERSION_MIN_IPHONEOS, LC_SOURCE_VERSION,
			LC_MAIN, LC_LOAD_DYLINKER, LC_LOAD_WEAK_DYLIB, LC_REEXPORT_DYLIB, LC_RPATH, LC_ID_DYLIB,
			LC_SYMSEG, LC_LOADFVMLIB, LC_IDFVMLIB, LC_IDENT, LC_FVMFILE, LC_PREPAGE, LC_ID_DYLINKER,
			LC_ROUTINES, LC_SUB_FRAMEWORK, LC_SUB_UMBRELLA, LC_SUB_CLIENT, LC_SUB_LIBRARY, LC_TWOLEVEL_HINTS,
			LC_PREBIND_CKSUM, LC_ROUTINES_64, LC_LAZY_LOAD_DYLIB, LC_LOAD_UPWARD_DYLIB, LC_DYLD_ENVIRONMENT,
			LC_LINKER_OPTION, LC_LINKER_OPTIMIZATION_HINT, LC_VERSION_MIN_TVOS, LC_VERSION_MIN_WATCHOS,
			LC_VERSION_NOTE, LC_BUILD_VERSION:
			// Nothing to update
		default:
			err = fmt.Errorf("unknown load command 0x%x (%s)", int(cmd.Cmd), cmd.Cmd)
		}
		if err != nil {
			return err
		}
	}
	// Do the final update of the DWARF segment's load command.
	return machoUpdateDwarfHeader(&reader, compressedSects, dwarfsize, dwarfstart, realdwarf)
}

// machoCompressSections tries to compress the DWARF segments in dwarfm,
// returning the updated sections and segment contents, nils if the sections
// weren't compressed, or an error if there was a problem reading dwarfm.
func machoCompressSections(ctxt *Link, dwarfm *macho.File) ([]*macho.Section, []byte, error) {
	if !ctxt.compressDWARF {
		return nil, nil, nil
	}

	dwarfseg := dwarfm.Segment("__DWARF")
	var sects []*macho.Section
	var buf bytes.Buffer

	for _, sect := range dwarfm.Sections {
		if sect.Seg != "__DWARF" {
			continue
		}

		// As of writing, there are no relocations in dsymutil's output
		// so there's no point in worrying about them. Bail out if that
		// changes.
		if sect.Nreloc != 0 {
			return nil, nil, nil
		}

		data, err := sect.Data()
		if err != nil {
			return nil, nil, err
		}

		compressed, contents, err := machoCompressSection(data)
		if err != nil {
			return nil, nil, err
		}

		newSec := *sect
		newSec.Offset = uint32(dwarfseg.Offset) + uint32(buf.Len())
		newSec.Addr = dwarfseg.Addr + uint64(buf.Len())
		if compressed {
			newSec.Name = "__z" + sect.Name[2:]
			newSec.Size = uint64(len(contents))
		}
		sects = append(sects, &newSec)
		buf.Write(contents)
	}
	return sects, buf.Bytes(), nil
}

// machoCompressSection compresses secBytes if it results in less data.
func machoCompressSection(sectBytes []byte) (compressed bool, contents []byte, err error) {
	var buf bytes.Buffer
	buf.WriteString("ZLIB")
	var sizeBytes [8]byte
	binary.BigEndian.PutUint64(sizeBytes[:], uint64(len(sectBytes)))
	buf.Write(sizeBytes[:])

	z := zlib.NewWriter(&buf)
	if _, err := z.Write(sectBytes); err != nil {
		return false, nil, err
	}
	if err := z.Close(); err != nil {
		return false, nil, err
	}
	if buf.Len() >= len(sectBytes) {
		return false, sectBytes, nil
	}
	return true, buf.Bytes(), nil
}

// machoUpdateSegment updates the load command for a moved segment.
// Only the linkedit segment should move, and it should have 0 sections.
func machoUpdateSegment(r loadCmdReader, linkseg *macho.Segment, linkoffset uint64) error {
	var seg macho.Segment64
	if err := r.ReadAt(0, &seg); err != nil {
		return err
	}

	// Only the linkedit segment moved, anything before that is fine.
	if seg.Offset < linkseg.Offset {
		return nil
	}
	seg.Offset += linkoffset
	if err := r.WriteAt(0, &seg); err != nil {
		return err
	}
	// There shouldn't be any sections, but just to make sure...
	return machoUpdateSections(r, &seg, linkoffset, nil)
}

func machoUpdateSections(r loadCmdReader, seg *macho.Segment64, deltaOffset uint64, compressedSects []*macho.Section) error {
	nsect := seg.Nsect
	if nsect == 0 {
		return nil
	}
	sectOffset := int64(unsafe.Sizeof(*seg))

	var sect macho.Section64
	sectSize := int64(unsafe.Sizeof(sect))
	for i := uint32(0); i < nsect; i++ {
		if err := r.ReadAt(sectOffset, &sect); err != nil {
			return err
		}
		if compressedSects != nil {
			cSect := compressedSects[i]
			copy(sect.Name[:], cSect.Name)
			sect.Size = cSect.Size
			if cSect.Offset != 0 {
				sect.Offset = cSect.Offset + uint32(deltaOffset)
			}
			if cSect.Addr != 0 {
				sect.Addr = cSect.Addr
			}
		} else {
			if sect.Offset != 0 {
				sect.Offset += uint32(deltaOffset)
			}
			if sect.Reloff != 0 {
				sect.Reloff += uint32(deltaOffset)
			}
		}
		if err := r.WriteAt(sectOffset, &sect); err != nil {
			return err
		}
		sectOffset += sectSize
	}
	return nil
}

// machoUpdateDwarfHeader updates the DWARF segment load command.
func machoUpdateDwarfHeader(r *loadCmdReader, compressedSects []*macho.Section, dwarfsize uint64, dwarfstart int64, realdwarf *macho.Segment) error {
	cmd, err := r.Next()
	if err != nil {
		return err
	}
	if cmd.Cmd != macho.LoadCmdSegment64 {
		panic("not a Segment64")
	}
	var seg macho.Segment64
	if err := r.ReadAt(0, &seg); err != nil {
		return err
	}
	seg.Offset = uint64(dwarfstart)

	if compressedSects != nil {
		var segSize uint64
		for _, newSect := range compressedSects {
			segSize += newSect.Size
		}
		seg.Filesz = segSize
	} else {
		seg.Filesz = dwarfsize
	}

	// We want the DWARF segment to be considered non-loadable, so
	// force vmaddr and vmsize to zero. In addition, set the initial
	// protection to zero so as to make the dynamic loader happy,
	// since otherwise it may complain that the vm size and file
	// size don't match for the segment. See issues 21647 and 32673
	// for more context. Also useful to refer to the Apple dynamic
	// loader source, specifically ImageLoaderMachO::sniffLoadCommands
	// in ImageLoaderMachO.cpp (various versions can be found online, see
	// https://opensource.apple.com/source/dyld/dyld-519.2.2/src/ImageLoaderMachO.cpp.auto.html
	// as one example).
	seg.Addr = 0
	seg.Memsz = 0
	seg.Prot = 0

	if err := r.WriteAt(0, &seg); err != nil {
		return err
	}
	return machoUpdateSections(*r, &seg, uint64(dwarfstart)-realdwarf.Offset, compressedSects)
}

func machoUpdateLoadCommand(r loadCmdReader, linkseg *macho.Segment, linkoffset uint64, cmd interface{}, fields ...string) error {
	if err := r.ReadAt(0, cmd); err != nil {
		return err
	}
	value := reflect.Indirect(reflect.ValueOf(cmd))

	for _, name := range fields {
		field := value.FieldByName(name)
		if fieldval := field.Uint(); fieldval >= linkseg.Offset {
			field.SetUint(fieldval + linkoffset)
		}
	}
	if err := r.WriteAt(0, cmd); err != nil {
		return err
	}
	return nil
}
