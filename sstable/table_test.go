// Copyright 2011 The LevelDB-Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sstable

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/petermattis/pebble/bloom"
	"github.com/petermattis/pebble/db"
	"github.com/petermattis/pebble/storage"
)

// nonsenseWords are words that aren't in ../testdata/h.txt.
var nonsenseWords = []string{
	// Edge cases.
	"",
	"\x00",
	"\xff",
	"`",
	"a\x00",
	"aaaaaa",
	"pol\x00nius",
	"youth\x00",
	"youti",
	"zzzzzz",
	// Capitalized versions of actual words in ../testdata/h.txt.
	"A",
	"Hamlet",
	"thEE",
	"YOUTH",
	// The following were generated by http://soybomb.com/tricks/words/
	"pectures",
	"exectly",
	"tricatrippian",
	"recens",
	"whiratroce",
	"troped",
	"balmous",
	"droppewry",
	"toilizing",
	"crocias",
	"eathrass",
	"cheakden",
	"speablett",
	"skirinies",
	"prefing",
	"bonufacision",
}

var (
	wordCount = map[string]string{}
	minWord   = ""
	maxWord   = ""
)

func init() {
	f, err := os.Open(filepath.FromSlash("../testdata/h.txt"))
	if err != nil {
		panic(err)
	}
	defer f.Close()
	r := bufio.NewReader(f)

	for first := true; ; {
		s, err := r.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}
		k := strings.TrimSpace(string(s[8:]))
		v := strings.TrimSpace(string(s[:8]))
		wordCount[k] = v

		if first {
			first = false
			minWord = k
			maxWord = k
			continue
		}
		if minWord > k {
			minWord = k
		}
		if maxWord < k {
			maxWord = k
		}
	}

	if len(wordCount) != 1710 {
		panic(fmt.Sprintf("h.txt entry count: got %d, want %d", len(wordCount), 1710))
	}

	for _, s := range nonsenseWords {
		if _, ok := wordCount[s]; ok {
			panic(fmt.Sprintf("nonsense word %q was in h.txt", s))
		}
	}
}

func check(f storage.File, fp db.FilterPolicy) error {
	r := NewReader(f, 0, &db.Options{
		FilterPolicy:    fp,
		VerifyChecksums: true,
	})

	// Check that each key/value pair in wordCount is also in the table.
	for k, v := range wordCount {
		// Check using Get.
		if v1, err := r.get([]byte(k), nil); string(v1) != string(v) || err != nil {
			return fmt.Errorf("Get %q: got (%q, %v), want (%q, %v)", k, v1, err, v, error(nil))
		} else if len(v1) != cap(v1) {
			return fmt.Errorf("Get %q: len(v1)=%d, cap(v1)=%d", k, len(v1), cap(v1))
		}

		// Check using Find.
		i := r.NewIter(nil)
		i.SeekGE([]byte(k))
		if !i.Valid() || string(i.Key().UserKey) != k {
			return fmt.Errorf("Find %q: key was not in the table", k)
		}
		if k1 := i.Key().UserKey; len(k1) != cap(k1) {
			return fmt.Errorf("Find %q: len(k1)=%d, cap(k1)=%d", k, len(k1), cap(k1))
		}
		if string(i.Value()) != v {
			return fmt.Errorf("Find %q: got value %q, want %q", k, i.Value(), v)
		}
		if v1 := i.Value(); len(v1) != cap(v1) {
			return fmt.Errorf("Find %q: len(v1)=%d, cap(v1)=%d", k, len(v1), cap(v1))
		}
		if err := i.Close(); err != nil {
			return err
		}
	}

	// Check that nonsense words are not in the table.
	for _, s := range nonsenseWords {
		// Check using Get.
		if _, err := r.get([]byte(s), nil); err != db.ErrNotFound {
			return fmt.Errorf("Get %q: got %v, want ErrNotFound", s, err)
		}

		// Check using Find.
		i := r.NewIter(nil)
		i.SeekGE([]byte(s))
		if i.Valid() && s == string(i.Key().UserKey) {
			return fmt.Errorf("Find %q: unexpectedly found key in the table", s)
		}
		if err := i.Close(); err != nil {
			return err
		}
	}

	// Check that the number of keys >= a given start key matches the expected number.
	var countTests = []struct {
		count int
		start string
	}{
		// cat h.txt | cut -c 9- | wc -l gives 1710.
		{1710, ""},
		// cat h.txt | cut -c 9- | grep -v "^[a-b]" | wc -l gives 1522.
		{1522, "c"},
		// cat h.txt | cut -c 9- | grep -v "^[a-j]" | wc -l gives 940.
		{940, "k"},
		// cat h.txt | cut -c 9- | grep -v "^[a-x]" | wc -l gives 12.
		{12, "y"},
		// cat h.txt | cut -c 9- | grep -v "^[a-z]" | wc -l gives 0.
		{0, "~"},
	}
	for _, ct := range countTests {
		n, i := 0, r.NewIter(nil)
		for i.SeekGE([]byte(ct.start)); i.Valid(); i.Next() {
			n++
		}
		if n != ct.count {
			return fmt.Errorf("count %q: got %d, want %d", ct.start, n, ct.count)
		}
		n = 0
		for i.Last(); i.Valid(); i.Prev() {
			if bytes.Compare(i.Key().UserKey, []byte(ct.start)) < 0 {
				break
			}
			n++
		}
		if n != ct.count {
			return fmt.Errorf("count %q: got %d, want %d", ct.start, n, ct.count)
		}
		if err := i.Close(); err != nil {
			return err
		}
	}

	return r.Close()
}

var (
	memFileSystem = storage.NewMem()
	tmpFileCount  int
)

func build(compression db.Compression, fp db.FilterPolicy) (storage.File, error) {
	// Create a sorted list of wordCount's keys.
	keys := make([]string, len(wordCount))
	i := 0
	for k := range wordCount {
		keys[i] = k
		i++
	}
	sort.Strings(keys)

	// Write the key/value pairs to a new table, in increasing key order.
	filename := fmt.Sprintf("/tmp%d", tmpFileCount)
	f0, err := memFileSystem.Create(filename)
	if err != nil {
		return nil, err
	}
	defer f0.Close()
	tmpFileCount++
	w := NewWriter(f0, &db.Options{
		Compression:  compression,
		FilterPolicy: fp,
	})
	for _, k := range keys {
		v := wordCount[k]
		ikey := db.MakeInternalKey([]byte(k), 0, db.InternalKeyKindSet)
		if err := w.Add(ikey, []byte(v)); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	// Re-open that filename for reading.
	f1, err := memFileSystem.Open(filename)
	if err != nil {
		return nil, err
	}
	return f1, nil
}

func testReader(t *testing.T, filename string, fp db.FilterPolicy) {
	// Check that we can read a pre-made table.
	f, err := os.Open(filepath.FromSlash("../testdata/" + filename))
	if err != nil {
		t.Error(err)
		return
	}
	err = check(f, fp)
	if err != nil {
		t.Error(err)
		return
	}
}

func TestReaderDefaultCompression(t *testing.T) { testReader(t, "h.sst", nil) }
func TestReaderNoCompression(t *testing.T)      { testReader(t, "h.no-compression.sst", nil) }
func TestReaderBlockBloomIgnored(t *testing.T)  { testReader(t, "h.block-bloom.no-compression.sst", nil) }
func TestReaderFullBloomIgnored(t *testing.T)   { testReader(t, "h.full-bloom.no-compression.sst", nil) }

func TestReaderBloomUsed(t *testing.T) {
	// wantActualNegatives is the minimum number of nonsense words (i.e. false
	// positives or true negatives) to run through our filter. Some nonsense
	// words might be rejected even before the filtering step, if they are out
	// of the [minWord, maxWord] range of keys in the table.
	wantActualNegatives := 0
	for _, s := range nonsenseWords {
		if minWord < s && s < maxWord {
			wantActualNegatives++
		}
	}

	files := []string{
		"h.block-bloom.no-compression.sst",
		// TODO(peter): enable when table-filters are supported
		// "h.full-bloom.no-compression.sst",
	}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			for _, degenerate := range []bool{false, true} {
				c := &countingFilterPolicy{
					FilterPolicy: bloom.FilterPolicy(10),
					degenerate:   degenerate,
				}
				testReader(t, f, c)

				if c.truePositives != len(wordCount) {
					t.Errorf("degenerate=%t: true positives: got %d, want %d", degenerate, c.truePositives, len(wordCount))
				}
				if c.falseNegatives != 0 {
					t.Errorf("degenerate=%t: false negatives: got %d, want %d", degenerate, c.falseNegatives, 0)
				}

				if got := c.falsePositives + c.trueNegatives; got < wantActualNegatives {
					t.Errorf("degenerate=%t: actual negatives (false positives + true negatives): "+
						"got %d (%d + %d), want >= %d",
						degenerate, got, c.falsePositives, c.trueNegatives, wantActualNegatives)
				}

				if !degenerate {
					// The true negative count should be much greater than the false
					// positive count.
					if c.trueNegatives < 10*c.falsePositives {
						t.Errorf("degenerate=%t: true negative to false positive ratio (%d:%d) is too small",
							degenerate, c.trueNegatives, c.falsePositives)
					}
				}
			}
		})
	}
}

func TestBloomFilterFalsePositiveRate(t *testing.T) {
	f, err := os.Open(filepath.FromSlash("../testdata/h.block-bloom.no-compression.sst"))
	if err != nil {
		t.Fatal(err)
	}
	c := &countingFilterPolicy{
		FilterPolicy: bloom.FilterPolicy(1),
	}
	r := NewReader(f, 0, &db.Options{
		FilterPolicy: c,
	})

	const n = 10000
	// key is a buffer that will be re-used for n Get calls, each with a
	// different key. The "m" in the 2-byte prefix means that the key falls in
	// the [minWord, maxWord] range and so will not be rejected prior to
	// applying the Bloom filter. The "!" in the 2-byte prefix means that the
	// key is not actually in the table. The filter will only see actual
	// negatives: false positives or true negatives.
	key := []byte("m!....")
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(key[2:6], uint32(i))
		r.get(key, nil)
	}

	if c.truePositives != 0 {
		t.Errorf("true positives: got %d, want 0", c.truePositives)
	}
	if c.falseNegatives != 0 {
		t.Errorf("false negatives: got %d, want 0", c.falseNegatives)
	}
	if got := c.falsePositives + c.trueNegatives; got != n {
		t.Errorf("actual negatives (false positives + true negatives): got %d (%d + %d), want %d",
			got, c.falsePositives, c.trueNegatives, n)
	}

	// According the the comments in the C++ LevelDB code, the false positive
	// rate should be approximately 1% for for bloom.FilterPolicy(10). The 10
	// was the parameter used to write the .sst file. When reading the file,
	// the 1 in the bloom.FilterPolicy(1) above doesn't matter, only the
	// bloom.FilterPolicy matters.
	if got := float64(100*c.falsePositives) / n; got < 0.2 || 5 < got {
		t.Errorf("false positive rate: got %v%%, want approximately 1%%", got)
	}
}

type countingFilterPolicy struct {
	db.FilterPolicy
	degenerate bool

	truePositives  int
	falsePositives int
	falseNegatives int
	trueNegatives  int
}

func (c *countingFilterPolicy) MayContain(filter, key []byte) bool {
	got := true
	if c.degenerate {
		// When degenerate is true, we override the embedded FilterPolicy's
		// MayContain method to always return true. Doing so is a valid, if
		// inefficient, implementation of the FilterPolicy interface.
	} else {
		got = c.FilterPolicy.MayContain(filter, key)
	}
	_, want := wordCount[string(key)]

	switch {
	case got && want:
		c.truePositives++
	case got && !want:
		c.falsePositives++
	case !got && want:
		c.falseNegatives++
	case !got && !want:
		c.trueNegatives++
	}
	return got
}

func TestWriter(t *testing.T) {
	// Check that we can read a freshly made table.
	f, err := build(db.DefaultCompression, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = check(f, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func testNoCompressionOutput(t *testing.T, fp db.FilterPolicy) {
	filename := "../testdata/h.no-compression.sst"
	if fp != nil {
		filename = "../testdata/h.block-bloom.no-compression.sst"
	}

	// Check that a freshly made NoCompression table is byte-for-byte equal
	// to a pre-made table.
	want, err := ioutil.ReadFile(filepath.FromSlash(filename))
	if err != nil {
		t.Fatal(err)
	}

	f, err := build(db.NoCompression, fp)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, stat.Size())
	_, err = f.ReadAt(got, 0)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, want) {
		i := 0
		for ; i < len(got) && i < len(want) && got[i] == want[i]; i++ {
		}
		t.Fatalf("built table does not match pre-made table. From byte %d onwards,\ngot:\n% x\nwant:\n% x",
			i, got[i:], want[i:])
	}
}

// TODO(peter): these tests fail because we don't generate the
// rocksdb.properties block (yet).
//
// func TestNoCompressionOutput(t *testing.T)      { testNoCompressionOutput(t, nil) }
// func TestBloomNoCompressionOutput(t *testing.T) { testNoCompressionOutput(t, bloom.FilterPolicy(10)) }

func TestFinalBlockIsWritten(t *testing.T) {
	const blockSize = 100
	keys := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}
	valueLengths := []int{0, 1, 22, 28, 33, 40, 50, 61, 87, 100, 143, 200}
	xxx := bytes.Repeat([]byte("x"), valueLengths[len(valueLengths)-1])

	for nk := 0; nk <= len(keys); nk++ {
	loop:
		for _, vLen := range valueLengths {
			got, memFS := 0, storage.NewMem()

			wf, err := memFS.Create("foo")
			if err != nil {
				t.Errorf("nk=%d, vLen=%d: memFS create: %v", nk, vLen, err)
				continue
			}
			w := NewWriter(wf, &db.Options{
				BlockSize: blockSize,
			})
			for _, k := range keys[:nk] {
				if err := w.Add(db.InternalKey{UserKey: []byte(k)}, xxx[:vLen]); err != nil {
					t.Errorf("nk=%d, vLen=%d: set: %v", nk, vLen, err)
					continue loop
				}
			}
			if err := w.Close(); err != nil {
				t.Errorf("nk=%d, vLen=%d: writer close: %v", nk, vLen, err)
				continue
			}

			rf, err := memFS.Open("foo")
			if err != nil {
				t.Errorf("nk=%d, vLen=%d: memFS open: %v", nk, vLen, err)
				continue
			}
			r := NewReader(rf, 0, nil)
			i := r.NewIter(nil)
			for i.First(); i.Valid(); i.Next() {
				got++
			}
			if err := i.Close(); err != nil {
				t.Errorf("nk=%d, vLen=%d: Iterator close: %v", nk, vLen, err)
				continue
			}
			if err := r.Close(); err != nil {
				t.Errorf("nk=%d, vLen=%d: reader close: %v", nk, vLen, err)
				continue
			}

			if got != nk {
				t.Errorf("nk=%2d, vLen=%3d: got %2d keys, want %2d", nk, vLen, got, nk)
				continue
			}
		}
	}
}
