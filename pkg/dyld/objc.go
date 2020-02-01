package dyld

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/apex/log"
	"github.com/blacktop/ipsw/pkg/macho"
	"github.com/pkg/errors"
)

type optFlags uint32

const (
	isProduction              optFlags = (1 << 0) // never set in development cache
	noMissingWeakSuperclasses optFlags = (1 << 1) // never set in development cache
)

// Optimization structure
type Optimization struct {
	Version           uint32
	Flags             optFlags
	SelectorOptOffset int32
	HeaderOptRoOffset int32
	ClassOptOffset    int32
	ProtocolOptOffset int32
	HeaderOptRwOffset int32
}

func (o Optimization) isPointerAligned() bool {
	return (binary.Size(o) % 8) == 0
}

// Precomputed perfect hash table of strings.
// Base class for precomputed selector table and class table.
type stringHash struct {
	Capacity uint32
	Occupied uint32
	Shift    uint32
	Mask     uint32
	_        uint32 // was zero
	_        uint32 // alignment pad
	Salt     uint64
	Scramble [256]uint32
}

// StringHash struct
type StringHash struct {
	stringHash
	Tab        []byte  /* tab[mask+1] (always power-of-2) */
	CheckBytes []byte  /* check byte for each string */
	Offsets    []int32 /* offsets from &capacity to cstrings */
}

func (f *File) getLibObjC() (*macho.File, error) {
	image := f.Image("/usr/lib/libobjc.A.dylib")

	dat, err := image.Data()
	if err != nil {
		return nil, err
	}
	r := bytes.NewReader(dat)

	return macho.NewFile(r)
}

// GetSelectorAddress returns a selector's pointer address
func (f *File) GetSelectorAddress(selector string) (uint32, error) {

	// m, err := f.getLibObjC()
	image := f.Image("/usr/lib/libobjc.A.dylib")

	dat, err := image.Data()
	if err != nil {
		return 0, err
	}
	r := bytes.NewReader(dat)

	m, err := macho.NewFile(r)
	if err != nil {
		return 0, err
	}

	for _, s := range m.Sections {
		if s.Seg == "__TEXT" && s.Name == "__objc_opt_ro" {
			dat, err := s.Data()
			if err != nil {
				return 0, err
			}
			secReader := bytes.NewReader(dat)
			opt := Optimization{}
			if err := binary.Read(secReader, f.ByteOrder, &opt); err != nil {
				return 0, err
			}
			if opt.Version != 15 {
				return 0, fmt.Errorf("objc optimization version should be 15, but found %d", opt.Version)
			}
			fmt.Println("Objective-C Optimization:", opt)
			// TODO: what is this offset from ???
			r.Seek(int64(int32(s.Offset)+opt.SelectorOptOffset), io.SeekStart)

			shash := StringHash{}
			if err := binary.Read(r, f.ByteOrder, &shash.stringHash); err != nil {
				return 0, err
			}
			shash.Tab = make([]byte, shash.Mask+1)
			if err := binary.Read(r, f.ByteOrder, &shash.Tab); err != nil {
				return 0, err
			}
			shash.CheckBytes = make([]byte, shash.Capacity)
			if err := binary.Read(r, f.ByteOrder, &shash.CheckBytes); err != nil {
				return 0, err
			}
			shash.Offsets = make([]int32, shash.Capacity)
			if err := binary.Read(r, f.ByteOrder, &shash.Offsets); err != nil {
				return 0, err
			}

			ptr, err := shash.getIndex([]byte(selector))
			if err != nil {
				return 0, errors.Wrapf(err, "failed get selector address for %s", selector)
			}

			fmt.Printf("    0x%x: %s\n", ptr, selector)
			return ptr, nil
		}
	}

	return 0, fmt.Errorf("failed get selector address for %s", selector)
}

// Selectors returns all of the Objective-C selectors
func (f *File) Selectors(imageNames ...string) error {
	var mask uint64 = 0x7FFFFFFFFFFFF
	var images []*CacheImage

	libobjc, err := f.getLibObjC()
	if err != nil {
		return err
	}

	if len(imageNames) > 0 && len(imageNames[0]) > 0 {
		for _, imageName := range imageNames {
			images = append(images, f.Image(imageName))
		}
	} else {
		images = f.Images
	}
	fmt.Println("Objective-C Selectors:")
	for _, image := range images {
		fmt.Println(image.Name)
		m, err := image.GetMacho()
		if err != nil {
			return errors.Wrapf(err, "failed get image %s as MachO", image.Name)
		}
		for _, s := range m.Sections {
			if s.Seg == "__DATA" && s.Name == "__objc_selrefs" {
				selectorPtrs := make([]uint64, s.Size/8)
				if err := binary.Read(s.Open(), f.ByteOrder, &selectorPtrs); err != nil {
					return err
				}
				for idx, ptr := range selectorPtrs {
					selectorPtrs[idx] = ptr & mask
				}

				objcRoSeg := libobjc.Segment("__OBJC_RO")
				// if objcRoSeg == nil {
				// 	fmt.Println("  - No selectors.")
				// 	return fmt.Errorf("segment __OBJC_RO does not exist")
				// }
				sr := objcRoSeg.Open()
				for _, ptr := range selectorPtrs {
					sr.Seek(int64(ptr-objcRoSeg.Addr), io.SeekStart)
					s, err := bufio.NewReader(sr).ReadString('\x00')
					if err != nil {
						log.Error(errors.Wrapf(err, "failed to read selector name at: %d", ptr-objcRoSeg.Addr).Error())
					}
					fmt.Printf("    0x%x: %s\n", ptr, strings.Trim(s, "\x00"))
				}
			}
		}
		m.Close()
	}
	return nil
}

/*
--------------------------------------------------------------------
mix -- mix 3 64-bit values reversibly.
mix() takes 48 machine instructions, but only 24 cycles on a superscalar
  machine (like Intel's new MMX architecture).  It requires 4 64-bit
  registers for 4::2 parallelism.
All 1-bit deltas, all 2-bit deltas, all deltas composed of top bits of
  (a,b,c), and all deltas of bottom bits were tested.  All deltas were
  tested both on random keys and on keys that were nearly all zero.
  These deltas all cause every bit of c to change between 1/3 and 2/3
  of the time (well, only 113/400 to 287/400 of the time for some
  2-bit delta).  These deltas all cause at least 80 bits to change
  among (a,b,c) when the mix is run either forward or backward (yes it
  is reversible).
This implies that a hash using mix64 has no funnels.  There may be
  characteristics with 3-bit deltas or bigger, I didn't test for
  those.
--------------------------------------------------------------------
*/
func mix64(a, b, c *uint64) {
	*a = (*a - *b - *c) ^ (*c >> 43)
	*b = (*b - *c - *a) ^ (*a << 9)
	*c = (*c - *a - *b) ^ (*b >> 8)
	*a = (*a - *b - *c) ^ (*c >> 38)
	*b = (*b - *c - *a) ^ (*a << 23)
	*c = (*c - *a - *b) ^ (*b >> 5)
	*a = (*a - *b - *c) ^ (*c >> 35)
	*b = (*b - *c - *a) ^ (*a << 49)
	*c = (*c - *a - *b) ^ (*b >> 11)
	*a = (*a - *b - *c) ^ (*c >> 12)
	*b = (*b - *c - *a) ^ (*a << 18)
	*c = (*c - *a - *b) ^ (*b >> 22)
}

/*
--------------------------------------------------------------------
hash() -- hash a variable-length key into a 64-bit value
  k     : the key (the unaligned variable-length array of bytes)
  len   : the length of the key, counting by bytes
  level : can be any 8-byte value
Returns a 64-bit value.  Every bit of the key affects every bit of
the return value.  No funnels.  Every 1-bit and 2-bit delta achieves
avalanche.  About 41+5len instructions.

The best hash table sizes are powers of 2.  There is no need to do
mod a prime (mod is sooo slow!).  If you need less than 64 bits,
use a bitmask.  For example, if you need only 10 bits, do
  h = (h & hashmask(10));
In which case, the hash table should have hashsize(10) elements.

If you are hashing n strings (uint8_t **)k, do it like this:
  for (i=0, h=0; i<n; ++i) h = hash( k[i], len[i], h);

By Bob Jenkins, Jan 4 1997.  bob_jenkins@burtleburtle.net.  You may
use this code any way you wish, private, educational, or commercial,
but I would appreciate if you give me credit.

See http://burtleburtle.net/bob/hash/evahash.html
Use for hash table lookup, or anything where one collision in 2^^64
is acceptable.  Do NOT use for cryptographic purposes.
--------------------------------------------------------------------
*/

func lookup8(k []byte, level uint64) uint64 {
	// uint8_t *k;        /* the key */
	// uint64_t  length;   /* the length of the key */
	// uint64_t  level;    /* the previous hash, or an arbitrary value */
	var a, b, c uint64
	var length uint64

	/* Set up the internal state */
	length = uint64(len(k))
	a = level
	b = level              /* the previous hash value */
	c = 0x9e3779b97f4a7c13 /* the golden ratio; an arbitrary value */
	p := 0
	/*---------------------------------------- handle most of the key */
	for length >= 24 {
		a += uint64(k[p+0]) + (uint64(k[p+1]) << 8) + (uint64(k[p+2]) << 16) + (uint64(k[p+3]) << 24) + (uint64(k[p+4]) << 32) + (uint64(k[p+5]) << 40) + (uint64(k[p+6]) << 48) + (uint64(k[p+7]) << 56)
		b += uint64(k[p+8]) + (uint64(k[p+9]) << 8) + (uint64(k[p+10]) << 16) + (uint64(k[p+11]) << 24) + (uint64(k[p+12]) << 32) + (uint64(k[p+13]) << 40) + (uint64(k[p+14]) << 48) + (uint64(k[p+15]) << 56)
		c += uint64(k[p+16]) + (uint64(k[p+17]) << 8) + (uint64(k[p+18]) << 16) + (uint64(k[p+19]) << 24) + (uint64(k[p+20]) << 32) + (uint64(k[p+21]) << 40) + (uint64(k[p+22]) << 48) + (uint64(k[p+23]) << 56)
		mix64(&a, &b, &c)
		p += 24
		length -= 24
	}

	/*------------------------------------- handle the last 23 bytes */
	c += uint64(len(k))
	switch length { /* all the case statements fall through */
	case 23:
		c += (uint64(k[p+22]) << 56)
	case 22:
		c += (uint64(k[p+21]) << 48)
	case 21:
		c += (uint64(k[p+20]) << 40)
	case 20:
		c += (uint64(k[p+19]) << 32)
	case 19:
		c += (uint64(k[p+18]) << 24)
	case 18:
		c += (uint64(k[p+17]) << 16)
	case 17:
		c += (uint64(k[p+16]) << 8)
	/* the first byte of c is reserved for the length */
	case 16:
		b += (uint64(k[p+15]) << 56)
	case 15:
		b += (uint64(k[p+14]) << 48)
	case 14:
		b += (uint64(k[p+13]) << 40)
	case 13:
		b += (uint64(k[p+12]) << 32)
	case 12:
		b += (uint64(k[p+11]) << 24)
	case 11:
		b += (uint64(k[p+10]) << 16)
	case 10:
		b += (uint64(k[p+9]) << 8)
	case 9:
		b += (uint64(k[p+8]))
	case 8:
		a += (uint64(k[p+7]) << 56)
	case 7:
		a += (uint64(k[p+6]) << 48)
	case 6:
		a += (uint64(k[p+5]) << 40)
	case 5:
		a += (uint64(k[p+4]) << 32)
	case 4:
		a += (uint64(k[p+3]) << 24)
	case 3:
		a += (uint64(k[p+2]) << 16)
	case 2:
		a += (uint64(k[p+1]) << 8)
	case 1:
		a += uint64(k[p+0])
		/* case 0: nothing left to add */
	}
	mix64(&a, &b, &c)
	/*-------------------------------------------- report the result */
	return c
}

func (sh StringHash) hash(key []byte) uint32 {
	val := lookup8(key, sh.Salt)
	return uint32((val >> uint64(sh.Shift)) ^ uint64(sh.Scramble[sh.Tab[val&uint64(sh.Mask)]]))
}

// The check bytes are used to reject strings that aren't in the table
// without paging in the table's cstring data. This checkbyte calculation
// catches 4785/4815 rejects when launching Safari; a perfect checkbyte
// would catch 4796/4815.
func checkbyte(key []byte) uint8 {
	return ((key[0] & 0x7) << 5) | (uint8(len(key)) & 0x1f)
}

func (sh StringHash) getIndex(key []byte) (uint32, error) {
	h := sh.hash(key)
	// Use check byte to reject without paging in the table's cstrings
	hCheck := sh.CheckBytes[h]
	keyCheck := checkbyte(key)
	if hCheck != keyCheck {
		return 0, fmt.Errorf("INDEX_NOT_FOUND")
	}
	offset := sh.Offsets[h]
	if offset == 0 {
		return 0, fmt.Errorf("INDEX_NOT_FOUND")
	}
	// result = (const char *)this + offset
	// TODO: fix me
	result := "FIX ME"
	if result != string(key) {
		return 0, fmt.Errorf("INDEX_NOT_FOUND")
	}

	return h, nil
}
