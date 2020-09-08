// Copyright 2020 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package manifest

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
)

// Intervals are of the form [start, end) with no gap between intervals. Each
// file overlaps perfectly with a sequence of intervals. This perfect overlap
// occurs because the union of file boundary keys is used to pick intervals.
// However the largest key in a file is inclusive, so when it is used as
// an interval, the actual key is ImmediateSuccessor(key). We don't have the
// ImmediateSuccessor function to do this computation, so we instead keep an
// isLargest bool to remind the code about this fact. This is used for
// comparisons in the following manner:
// - intervalKey{k, false} < intervalKey{k, true}
// - k1 < k2 -> intervalKey{k1, _} < intervalKey{k2, _}.
//
// Note that the file's largest key is exclusive if the internal key
// has a trailer matching the rangedel sentinel key. In this case, we set
// isLargest to false for end interval computation.
//
// For example, consider three files with bounds [a,e], [b,g], and [e,j]. The
// interval keys produced would be intervalKey{a, false}, intervalKey{b, false},
// intervalKey{e, false}, intervalKey{e, true}, intervalKey{g, true} and
// intervalKey{j, true}, resulting in intervals
// [a, b), [b, (e, false)), [(e,false), (e, true)), [(e, true), (g, true)) and
// [(g, true), (j, true)). The first file overlaps with the first three
// perfectly, the second file overlaps with the second through to fourth
// intervals, and the third file overlaps with the last three.
//
// The intervals are indexed starting from 0, with the index of the interval
// being the index of the start key of the interval.
//
// In addition to helping with compaction picking, we use interval indices
// to assign each file an interval range once. Subsequent operations, say
// picking overlapping files for a compaction, only need to use the index
// numbers and so avoid expensive byte slice comparisons.
type intervalKey struct {
	key       []byte
	isLargest bool
}

func intervalKeyCompare(cmp Compare, a, b intervalKey) int {
	rv := cmp(a.key, b.key)
	if rv == 0 {
		if a.isLargest && !b.isLargest {
			return +1
		}
		if !a.isLargest && b.isLargest {
			return -1
		}
	}
	return rv
}

type intervalKeySorter struct {
	keys []intervalKey
	cmp  Compare
}

func (s intervalKeySorter) Len() int { return len(s.keys) }
func (s intervalKeySorter) Less(i, j int) bool {
	return intervalKeyCompare(s.cmp, s.keys[i], s.keys[j]) < 0
}
func (s intervalKeySorter) Swap(i, j int) {
	s.keys[i], s.keys[j] = s.keys[j], s.keys[i]
}

func sortAndDedup(keys []intervalKey, cmp Compare) []intervalKey {
	if len(keys) == 0 {
		return nil
	}
	sorter := intervalKeySorter{keys: keys, cmp: cmp}
	sort.Sort(sorter)
	j := 0
	for i := 1; i < len(keys); i++ {
		cmp := intervalKeyCompare(cmp, keys[i], keys[j])
		if cmp != 0 {
			j++
			keys[j] = keys[i]
		}
	}
	return keys[:j+1]
}

// A key interval of the form [start, end). The end is not represented here
// since it is implicit in the start of the next interval. The last interval is
// an exception but we don't need to ever lookup the end of that interval; the
// last fileInterval will only act as an end key marker. The set of intervals
// is const after initialization.
type fileInterval struct {
	index    int
	startKey intervalKey

	// True iff some file in this interval is compacting to base. Such intervals
	// cannot have any files participate in L0 -> Lbase compactions.
	isBaseCompacting bool

	// The min and max intervals index across all the files that overlap with this
	// interval. Inclusive on both sides.
	filesMinIntervalIndex int
	filesMaxIntervalIndex int

	// True if another interval that has a file extending into this interval is
	// undergoing a compaction into Lbase. In other words, this bool is true
	// if any interval in [filesMinIntervalIndex,
	// filesMaxIntervalIndex] has isBaseCompacting set to true. This
	// lets the compaction picker de-prioritize this interval for picking
	// compactions, since there's a high chance that a base compaction with a
	// sufficient height of sublevels rooted at this interval could not be
	// chosen due to the ongoing base compaction in the
	// other interval. If the file straddling the two intervals is at a
	// sufficiently high sublevel (with enough compactible files below it to
	// satisfy minCompactionDepth), this is not an issue, but to optimize for
	// quickly picking base compactions far away from other base compactions,
	// this bool is used as a heuristic (but not as a complete disqualifier).
	intervalRangeIsBaseCompacting bool

	// fileCount - compactingFileCount is the stack depth that requires
	// starting new compactions. This metric is not precise since the
	// compactingFileCount can include files that are part of N (where N > 1)
	// intra-L0 compactions, so the stack depth after those complete will be
	// fileCount - compactingFileCount + N. We ignore this imprecision since
	// we don't want to track which files are part of which intra-L0
	// compaction.
	fileCount           int
	compactingFileCount int

	// All files in this interval, in increasing sublevel order.
	files []*FileMetadata

	// Interpolated from files in this interval. For files spanning multiple
	// intervals, we assume an equal distribution of bytes across all those
	// intervals.
	estimatedBytes uint64
}

// Helper type for any cases requiring a bool slice.
type bitSet []bool

func newBitSet(n int) bitSet {
	return make([]bool, n)
}

func (b *bitSet) markBit(i int) {
	(*b)[i] = true
}

func (b *bitSet) markBits(start, end int) {
	for i := start; i < end; i++ {
		(*b)[i] = true
	}
}

func (b *bitSet) clearAllBits() {
	for i := range *b {
		(*b)[i] = false
	}
}

// L0Sublevels represents a sublevel view of SSTables in L0. Tables in one
// sublevel are non-overlapping in key ranges, and keys in higher-indexed
// sublevels shadow older versions in lower-indexed sublevels. These invariants
// are similar to the regular level invariants, except with higher indexed
// sublevels having newer keys as opposed to lower indexed levels.
//
// There is no limit to the number of sublevels that can exist in L0 at any
// time, however read and compaction performance is best when there are as few
// sublevels as possible.
type L0Sublevels struct {
	// Levels are ordered from oldest sublevel to youngest sublevel in the
	// outer slice, and the inner slice contains non-overlapping files for
	// that sublevel in increasing key order.
	Levels [][]*FileMetadata

	cmp       Compare
	formatKey base.FormatKey

	fileBytes uint64
	// All the L0 files, ordered from oldest to youngest.
	levelMetadata *LevelMetadata

	// The file intervals in increasing key order.
	orderedIntervals []fileInterval

	// Keys to break flushes at.
	flushSplitUserKeys [][]byte
}

func insertIntoSubLevel(files []*FileMetadata, f *FileMetadata) []*FileMetadata {
	index := sort.Search(len(files), func(i int) bool {
		return f.minIntervalIndex < files[i].minIntervalIndex
	})
	if index == len(files) {
		files = append(files, f)
		return files
	}
	files = append(files, nil)
	copy(files[index+1:], files[index:])
	files[index] = f
	return files
}

// NewL0Sublevels creates an L0Sublevels instance for a given set of L0 files.
// These files must all be in L0 and must be sorted by seqnum (see
// SortBySeqNum). During interval iteration, when flushSplitMaxBytes bytes are
// exceeded in the range of intervals since the last flush split key, a flush
// split key is added.
//
// This method can be called without DB.mu being held, so any DB.mu protected
// fields in FileMetadata cannot be accessed here, such as Compacting and
// IsIntraL0Compacting. Those fields are accessed in InitCompactingFileInfo
// instead.
func NewL0Sublevels(
	levelMetadata *LevelMetadata, cmp Compare, formatKey base.FormatKey, flushSplitMaxBytes int64,
) (*L0Sublevels, error) {
	s := &L0Sublevels{cmp: cmp, formatKey: formatKey}
	s.levelMetadata = levelMetadata
	keys := make([]intervalKey, 0, 2*s.levelMetadata.Len())
	iter := levelMetadata.Iter()
	for i, f := 0, iter.First(); f != nil; i, f = i+1, iter.Next() {
		f.l0Index = i
		keys = append(keys, intervalKey{key: f.Smallest.UserKey})
		keys = append(keys, intervalKey{
			key:       f.Largest.UserKey,
			isLargest: f.Largest.Trailer != base.InternalKeyRangeDeleteSentinel,
		})
	}
	keys = sortAndDedup(keys, cmp)
	// All interval indices reference s.orderedIntervals.
	s.orderedIntervals = make([]fileInterval, len(keys))
	for i := range keys {
		s.orderedIntervals[i] = fileInterval{
			index:                 i,
			startKey:              keys[i],
			filesMinIntervalIndex: i,
			filesMaxIntervalIndex: i,
		}
	}
	// Initialize minIntervalIndex and maxIntervalIndex for each file, and use that
	// to update intervals.
	for f := iter.First(); f != nil; f = iter.Next() {
		// Set f.minIntervalIndex and f.maxIntervalIndex.
		f.minIntervalIndex = sort.Search(len(keys), func(index int) bool {
			return intervalKeyCompare(cmp, intervalKey{key: f.Smallest.UserKey}, keys[index]) <= 0
		})
		if f.minIntervalIndex == len(keys) {
			return nil, errors.Errorf("expected sstable bound to be in interval keys: %s", f.Smallest.UserKey)
		}
		f.maxIntervalIndex = sort.Search(len(keys), func(index int) bool {
			return intervalKeyCompare(
				cmp, intervalKey{key: f.Largest.UserKey, isLargest: f.Largest.Trailer != base.InternalKeyRangeDeleteSentinel}, keys[index]) <= 0
		})
		if f.maxIntervalIndex == len(keys) {
			return nil, errors.Errorf("expected sstable bound to be in interval keys: %s", f.Largest.UserKey)
		}
		f.maxIntervalIndex--
		// This is a simple and not very accurate estimate of the number of
		// bytes this SSTable contributes to the intervals it is a part of.
		//
		// TODO(bilal): Call EstimateDiskUsage in sstable.Reader with interval
		// bounds to get a better estimate for each interval.
		interpolatedBytes := f.Size / uint64(f.maxIntervalIndex-f.minIntervalIndex+1)
		s.fileBytes += f.Size
		subLevel := 0
		// Update state in every fileInterval for this file.
		for i := f.minIntervalIndex; i <= f.maxIntervalIndex; i++ {
			interval := &s.orderedIntervals[i]
			if len(interval.files) > 0 &&
				subLevel <= interval.files[len(interval.files)-1].subLevel {
				subLevel = interval.files[len(interval.files)-1].subLevel + 1
			}
			s.orderedIntervals[i].fileCount++
			interval.estimatedBytes += interpolatedBytes
			if f.minIntervalIndex < interval.filesMinIntervalIndex {
				interval.filesMinIntervalIndex = f.minIntervalIndex
			}
			if f.maxIntervalIndex > interval.filesMaxIntervalIndex {
				interval.filesMaxIntervalIndex = f.maxIntervalIndex
			}
		}
		for i := f.minIntervalIndex; i <= f.maxIntervalIndex; i++ {
			interval := &s.orderedIntervals[i]
			interval.files = append(interval.files, f)
		}
		f.subLevel = subLevel
		if subLevel > len(s.Levels) {
			return nil, errors.Errorf("chose a sublevel beyond allowed range of sublevels: %d vs 0-%d", subLevel, len(s.Levels))
		}
		if subLevel == len(s.Levels) {
			s.Levels = append(s.Levels, []*FileMetadata{f})
		} else {
			s.Levels[subLevel] = insertIntoSubLevel(s.Levels[subLevel], f)
		}
	}
	var cumulativeBytes uint64
	// Multiply flushSplitMaxBytes by the number of sublevels. This prevents
	// excessive flush splitting when the number of sublevels increases.
	flushSplitMaxBytes *= int64(len(s.Levels))
	for i := 0; i < len(s.orderedIntervals); i++ {
		interval := &s.orderedIntervals[i]
		if flushSplitMaxBytes > 0 && cumulativeBytes > uint64(flushSplitMaxBytes) &&
			(len(s.flushSplitUserKeys) == 0 ||
				!bytes.Equal(interval.startKey.key, s.flushSplitUserKeys[len(s.flushSplitUserKeys)-1])) {
			s.flushSplitUserKeys = append(s.flushSplitUserKeys, interval.startKey.key)
			cumulativeBytes = 0
		}
		cumulativeBytes += s.orderedIntervals[i].estimatedBytes
	}
	return s, nil
}

// L0Compaction describes an active compaction with inputs from L0.
type L0Compaction struct {
	Smallest  InternalKey
	Largest   InternalKey
	IsIntraL0 bool
}

// InitCompactingFileInfo initializes internal flags relating to compacting
// files. Must be called after sublevel initialization.
//
// Requires DB.mu to be held.
func (s *L0Sublevels) InitCompactingFileInfo(inProgress []L0Compaction) {
	for i := range s.orderedIntervals {
		s.orderedIntervals[i].compactingFileCount = 0
		s.orderedIntervals[i].isBaseCompacting = false
		s.orderedIntervals[i].intervalRangeIsBaseCompacting = false
	}

	iter := s.levelMetadata.Iter()
	for f := iter.First(); f != nil; f = iter.Next() {
		if !f.Compacting {
			continue
		}
		for i := f.minIntervalIndex; i <= f.maxIntervalIndex; i++ {
			interval := &s.orderedIntervals[i]
			interval.compactingFileCount++
			if !f.IsIntraL0Compacting {
				// If f.Compacting && !f.IsIntraL0Compacting, this file is
				// being compacted to Lbase.
				interval.isBaseCompacting = true
			}
		}
	}

	// Some intervals may be base compacting without the files contained
	// within those intervals being marked as compacting. This is possible if
	// the files were added after the compaction initiated, and the active
	// compaction files straddle the input file. Mark these intervals as base
	// compacting.
	for _, c := range inProgress {
		startIK := intervalKey{key: c.Smallest.UserKey, isLargest: false}
		endIK := intervalKey{key: c.Largest.UserKey, isLargest: c.Largest.Trailer != base.InternalKeyRangeDeleteSentinel}
		start := sort.Search(len(s.orderedIntervals), func(i int) bool {
			return intervalKeyCompare(s.cmp, s.orderedIntervals[i].startKey, startIK) >= 0
		})
		end := sort.Search(len(s.orderedIntervals), func(i int) bool {
			return intervalKeyCompare(s.cmp, s.orderedIntervals[i].startKey, endIK) >= 0
		})
		for i := start; i < end && i < len(s.orderedIntervals); i++ {
			interval := &s.orderedIntervals[i]
			if !c.IsIntraL0 {
				interval.isBaseCompacting = true
			}
		}
	}

	min := 0
	for i := range s.orderedIntervals {
		interval := &s.orderedIntervals[i]
		if interval.isBaseCompacting {
			minIndex := interval.filesMinIntervalIndex
			if minIndex < min {
				minIndex = min
			}
			for j := minIndex; j <= interval.filesMaxIntervalIndex; j++ {
				min = j
				s.orderedIntervals[j].intervalRangeIsBaseCompacting = true
			}
		}
	}
}

// String produces a string containing useful debug information. Useful in test
// code and debugging.
func (s *L0Sublevels) String() string {
	return s.describe(false)
}

func (s *L0Sublevels) describe(verbose bool) string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "file count: %d, sublevels: %d, intervals: %d\nflush split keys(%d): [",
		s.levelMetadata.Len(), len(s.Levels), len(s.orderedIntervals), len(s.flushSplitUserKeys))
	for i := range s.flushSplitUserKeys {
		fmt.Fprintf(&buf, "%s", s.formatKey(s.flushSplitUserKeys[i]))
		if i < len(s.flushSplitUserKeys)-1 {
			fmt.Fprintf(&buf, ", ")
		}
	}
	fmt.Fprintln(&buf, "]")
	numCompactingFiles := 0
	for i := len(s.Levels) - 1; i >= 0; i-- {
		maxIntervals := 0
		sumIntervals := 0
		var totalBytes uint64
		for _, f := range s.Levels[i] {
			intervals := f.maxIntervalIndex - f.minIntervalIndex + 1
			if intervals > maxIntervals {
				maxIntervals = intervals
			}
			sumIntervals += intervals
			totalBytes += f.Size
			if f.Compacting {
				numCompactingFiles++
			}
		}
		fmt.Fprintf(&buf, "0.%d: file count: %d, bytes: %d, width (mean, max): %0.1f, %d, interval range: [%d, %d]\n",
			i, len(s.Levels[i]), totalBytes, float64(sumIntervals)/float64(len(s.Levels[i])), maxIntervals, s.Levels[i][0].minIntervalIndex,
			s.Levels[i][len(s.Levels[i])-1].maxIntervalIndex)
		for _, f := range s.Levels[i] {
			intervals := f.maxIntervalIndex - f.minIntervalIndex + 1
			if verbose {
				fmt.Fprintf(&buf, "\t%s\n", f)
			}
			if s.levelMetadata.Len() > 50 && intervals*3 > len(s.orderedIntervals) {
				var intervalsBytes uint64
				for k := f.minIntervalIndex; k <= f.maxIntervalIndex; k++ {
					intervalsBytes += s.orderedIntervals[k].estimatedBytes
				}
				fmt.Fprintf(&buf, "wide file: %d, [%d, %d], byte fraction: %f\n",
					f.FileNum, f.minIntervalIndex, f.maxIntervalIndex,
					float64(intervalsBytes)/float64(s.fileBytes))
			}
		}
	}

	lastCompactingIntervalStart := -1
	fmt.Fprintf(&buf, "compacting file count: %d, base compacting intervals: ", numCompactingFiles)
	i := 0
	foundBaseCompactingIntervals := false
	for ; i < len(s.orderedIntervals); i++ {
		interval := &s.orderedIntervals[i]
		if interval.fileCount == 0 {
			continue
		}
		if !interval.isBaseCompacting {
			if lastCompactingIntervalStart != -1 {
				if foundBaseCompactingIntervals {
					buf.WriteString(", ")
				}
				fmt.Fprintf(&buf, "[%d, %d]", lastCompactingIntervalStart, i-1)
				foundBaseCompactingIntervals = true
			}
			lastCompactingIntervalStart = -1
		} else {
			if lastCompactingIntervalStart == -1 {
				lastCompactingIntervalStart = i
			}
		}
	}
	if lastCompactingIntervalStart != -1 {
		if foundBaseCompactingIntervals {
			buf.WriteString(", ")
		}
		fmt.Fprintf(&buf, "[%d, %d]", lastCompactingIntervalStart, i-1)
	} else if !foundBaseCompactingIntervals {
		fmt.Fprintf(&buf, "none")
	}
	fmt.Fprintln(&buf, "")
	return buf.String()
}

// ReadAmplification returns the contribution of L0Sublevels to the read
// amplification for any particular point key. It is the maximum height of any
// tracked fileInterval. This is always less than or equal to the number of
// sublevels.
func (s *L0Sublevels) ReadAmplification() int {
	amp := 0
	for i := range s.orderedIntervals {
		interval := &s.orderedIntervals[i]
		if amp < interval.fileCount {
			amp = interval.fileCount
		}
	}
	return amp
}

// FlushSplitKeys returns a slice of user keys to split flushes at.
// Used by flushes to avoid writing sstables that straddle these split keys.
// These should be interpreted as the keys to start the next sstable (not the
// last key to include in the prev sstable). These are user keys so that
// range tombstones can be properly truncated (untruncated range tombstones
// are not permitted for L0 files).
func (s *L0Sublevels) FlushSplitKeys() [][]byte {
	return s.flushSplitUserKeys
}

// MaxDepthAfterOngoingCompactions returns an estimate of maximum depth of
// sublevels after all ongoing compactions run to completion. Used by compaction
// picker to decide compaction score for L0. There is no scoring for intra-L0
// compactions -- they only run if L0 score is high but we're unable to pick an
// L0 -> Lbase compaction.
func (s *L0Sublevels) MaxDepthAfterOngoingCompactions() int {
	depth := 0
	for i := range s.orderedIntervals {
		interval := &s.orderedIntervals[i]
		intervalDepth := interval.fileCount - interval.compactingFileCount
		if depth < intervalDepth {
			depth = intervalDepth
		}
	}
	return depth
}

// Only for temporary debugging in the absence of proper tests.
//
// TODO(bilal): Simplify away the debugging statements in this method, and make
// this a pure sanity checker.
func (s *L0Sublevels) checkCompaction(c *L0CompactionFiles) error {
	includedFiles := newBitSet(s.levelMetadata.Len())
	fileIntervalsByLevel := make([]struct {
		min int
		max int
	}, len(s.Levels))
	for i := range fileIntervalsByLevel {
		fileIntervalsByLevel[i].min = math.MaxInt32
		fileIntervalsByLevel[i].max = 0
	}
	var topLevel int
	var increment int
	var limitReached func(int) bool
	if c.isIntraL0 {
		topLevel = len(s.Levels) - 1
		increment = +1
		limitReached = func(level int) bool {
			return level == len(s.Levels)
		}
	} else {
		topLevel = 0
		increment = -1
		limitReached = func(level int) bool {
			return level < 0
		}
	}
	for _, f := range c.Files {
		if fileIntervalsByLevel[f.subLevel].min > f.minIntervalIndex {
			fileIntervalsByLevel[f.subLevel].min = f.minIntervalIndex
		}
		if fileIntervalsByLevel[f.subLevel].max < f.maxIntervalIndex {
			fileIntervalsByLevel[f.subLevel].max = f.maxIntervalIndex
		}
		includedFiles.markBit(f.l0Index)
		if c.isIntraL0 {
			if topLevel > f.subLevel {
				topLevel = f.subLevel
			}
		} else {
			if topLevel < f.subLevel {
				topLevel = f.subLevel
			}
		}
	}
	min := fileIntervalsByLevel[topLevel].min
	max := fileIntervalsByLevel[topLevel].max
	for level := topLevel; !limitReached(level); level += increment {
		if fileIntervalsByLevel[level].min < min {
			min = fileIntervalsByLevel[level].min
		}
		if fileIntervalsByLevel[level].max > max {
			max = fileIntervalsByLevel[level].max
		}
		index := sort.Search(len(s.Levels[level]), func(i int) bool {
			return s.Levels[level][i].maxIntervalIndex >= min
		})
		// start := index
		for ; index < len(s.Levels[level]); index++ {
			f := s.Levels[level][index]
			if f.minIntervalIndex > max {
				break
			}
			if c.isIntraL0 && f.LargestSeqNum >= c.earliestUnflushedSeqNum {
				return errors.Errorf(
					"sstable %s in compaction has sequence numbers higher than the earliest unflushed seqnum %d: %d-%d",
					f.FileNum, c.earliestUnflushedSeqNum, f.SmallestSeqNum,
					f.LargestSeqNum)
			}
			if !includedFiles[f.l0Index] {
				var buf strings.Builder
				fmt.Fprintf(&buf, "bug %t, seed interval: %d: level %d, sl index %d, f.index %d, min %d, max %d, pre-min %d, pre-max %d, f.min %d, f.max %d, filenum: %d, isCompacting: %t\n%s\n",
					c.isIntraL0, c.seedInterval, level, index, f.l0Index, min, max, c.preExtensionMinInterval, c.preExtensionMaxInterval,
					f.minIntervalIndex, f.maxIntervalIndex,
					f.FileNum, f.Compacting, s)
				fmt.Fprintf(&buf, "files included:\n")
				for _, f := range c.Files {
					fmt.Fprintf(&buf, "filenum: %d, sl: %d, index: %d, [%d, %d]\n",
						f.FileNum, f.subLevel, f.l0Index, f.minIntervalIndex, f.maxIntervalIndex)
				}
				fmt.Fprintf(&buf, "files added:\n")
				for _, f := range c.filesAdded {
					fmt.Fprintf(&buf, "filenum: %d, sl: %d, index: %d, [%d, %d]\n",
						f.FileNum, f.subLevel, f.l0Index, f.minIntervalIndex, f.maxIntervalIndex)
				}
				return errors.New(buf.String())
			}
		}
	}
	return nil
}

// UpdateStateForStartedCompaction updates internal L0Sublevels state for a
// recently started compaction. isBase specifies if this is a base compaction;
// if false, this is assumed to be an intra-L0 compaction. The specified
// compaction must be involving L0 SSTables. It's assumed that the Compacting
// and IsIntraL0Compacting fields are already set on all FileMetadatas passed
// in.
func (s *L0Sublevels) UpdateStateForStartedCompaction(inputs []LevelSlice, isBase bool) error {
	minIntervalIndex := -1
	maxIntervalIndex := 0
	for i := range inputs {
		iter := inputs[i].Iter()
		for f := iter.First(); f != nil; f = iter.Next() {
			for i := f.minIntervalIndex; i <= f.maxIntervalIndex; i++ {
				interval := &s.orderedIntervals[i]
				interval.compactingFileCount++
			}
			if f.minIntervalIndex < minIntervalIndex || minIntervalIndex == -1 {
				minIntervalIndex = f.minIntervalIndex
			}
			if f.maxIntervalIndex > maxIntervalIndex {
				maxIntervalIndex = f.maxIntervalIndex
			}
		}
	}
	if isBase {
		for i := minIntervalIndex; i <= maxIntervalIndex; i++ {
			interval := &s.orderedIntervals[i]
			interval.isBaseCompacting = isBase
			for j := interval.filesMinIntervalIndex; j <= interval.filesMaxIntervalIndex; j++ {
				s.orderedIntervals[j].intervalRangeIsBaseCompacting = true
			}
		}
	}
	return nil
}

// L0CompactionFiles represents a candidate set of L0 files for compaction.
// Also referred to as "lcf". Contains state information useful
// for generating the compaction (such as Files), as well as for picking
// between candidate compactions (eg. fileBytes and
// seedIntervalStackDepthReduction).
type L0CompactionFiles struct {
	Files         []*FileMetadata
	FilesIncluded bitSet
	// A "seed interval" is an interval with a high stack depth that was chosen
	// to bootstrap this compaction candidate. seedIntervalStackDepthReduction
	// is the number of sublevels that have a file in the seed interval that is
	// a part of this compaction.
	seedIntervalStackDepthReduction int
	// For base compactions, seedIntervalMinLevel is 0, and for intra-L0
	// compactions, seedIntervalMaxLevel is len(s.Files)-1 i.e. the highest
	// sublevel.
	seedIntervalMinLevel int
	seedIntervalMaxLevel int
	// Index of the seed interval.
	seedInterval int
	// Sum of file sizes for all files in this compaction.
	fileBytes uint64
	// Intervals with index [minIntervalIndex, maxIntervalIndex] are
	// participating in this compaction; it's the union set of all intervals
	// overlapped by participating files.
	minIntervalIndex int
	maxIntervalIndex int

	// Set for intra-L0 compactions. SSTables with sequence numbers greater
	// than earliestUnflushedSeqNum cannot be a part of intra-L0 compactions.
	isIntraL0               bool
	earliestUnflushedSeqNum uint64

	// For debugging purposes only. Used in checkCompaction().
	preExtensionMinInterval int
	preExtensionMaxInterval int
	filesAdded              []*FileMetadata
}

// addFile adds the specified file to the LCF.
func (l *L0CompactionFiles) addFile(f *FileMetadata) {
	if l.FilesIncluded[f.l0Index] {
		return
	}
	l.FilesIncluded.markBit(f.l0Index)
	l.Files = append(l.Files, f)
	l.filesAdded = append(l.filesAdded, f)
	l.fileBytes += f.Size
	if f.minIntervalIndex < l.minIntervalIndex {
		l.minIntervalIndex = f.minIntervalIndex
	}
	if f.maxIntervalIndex > l.maxIntervalIndex {
		l.maxIntervalIndex = f.maxIntervalIndex
	}
}

// Helper to order intervals being considered for compaction.
type intervalAndScore struct {
	interval int
	score    int
}
type intervalSorterByDecreasingScore []intervalAndScore

func (is intervalSorterByDecreasingScore) Len() int { return len(is) }
func (is intervalSorterByDecreasingScore) Less(i, j int) bool {
	return is[i].score > is[j].score
}
func (is intervalSorterByDecreasingScore) Swap(i, j int) {
	is[i], is[j] = is[j], is[i]
}

// Compactions:
//
// The sub-levels and intervals can be visualized in 2 dimensions as the X
// axis containing intervals in increasing order and the Y axis containing
// sub-levels (older to younger). The intervals can be sparse wrt sub-levels.
// We observe that the system is typically under severe pressure in L0 during
// large numbers of ingestions where most files added to L0 are narrow and
// non-overlapping.
//
//    L0.1    d---g
//    L0.0  c--e  g--j o--s u--x
//
// As opposed to a case with a lot of wide, overlapping L0 files:
//
//    L0.3     d-----------r
//    L0.2    c--------o
//    L0.1   b-----------q
//    L0.0  a----------------x
//
// In that case we expect the rectangle represented in the good visualization
// above (i.e. the first one) to be wide and short, and not too sparse (most
// intervals will have fileCount close to the sub-level count), which would make
// it amenable to concurrent L0 -> Lbase compactions.
//
// L0 -> Lbase: The high-level goal of a L0 -> Lbase compaction is to reduce
// stack depth, by compacting files in the intervals with the highest
// (fileCount - compactingCount). Additionally, we would like compactions to
// not involve a huge number of files, so that they finish quickly, and to
// allow for concurrent L0 -> Lbase compactions when needed. In order to
// achieve these goals we would like compactions to visualize as capturing
// thin and tall rectangles. The approach below is to consider intervals in
// some order and then try to construct a compaction using the interval. The
// first interval we can construct a compaction for is the compaction that is
// started. There can be multiple heuristics in choosing the ordering of the
// intervals -- the code uses one heuristic that worked well for a large
// ingestion stemming from a cockroachdb import, but additional experimentation
// is necessary to pick a general heuristic. Additionally, the compaction that
// gets picked may be not as desirable as one that could be constructed later
// in terms of reducing stack depth (since adding more files to the compaction
// can get blocked by needing to encompass files that are already being
// compacted). So an alternative would be to try to construct more than one
// compaction and pick the best one.
//
// Here's a visualization of an ideal L0->LBase compaction selection:
//
//    L0.3  a--d    g-j
//    L0.2         f--j          r-t
//    L0.1   b-d  e---j
//    L0.0  a--d   f--j  l--o  p-----x
//
//    Lbase a--------i    m---------w
//
// The [g,j] interval has the highest stack depth, so it would have the highest
// priority for selecting a base compaction candidate. Assuming none of the
// files are already compacting, this is the compaction that will be chosen:
//
//               _______
//    L0.3  a--d |  g-j|
//    L0.2       | f--j|         r-t
//    L0.1   b-d |e---j|
//    L0.0  a--d | f--j| l--o  p-----x
//
//    Lbase a--------i    m---------w
//
// Note that running this compaction will mark the a--i file in Lbase as
// compacting, and when ExtendL0ForBaseCompactionTo is called with the bounds
// of that base file, it'll expand the compaction to also include all L0 files
// in the a-d interval. The resultant compaction would then be:
//
//         _____________
//    L0.3 |a--d    g-j|
//    L0.2 |       f--j|         r-t
//    L0.1 | b-d  e---j|
//    L0.0 |a--d   f--j| l--o  p-----x
//
//    Lbase a--------i    m---------w
//
// The next best interval for base compaction would therefore
// be the one including r--t in L0.2 and p--x in L0.0, and both this compaction
// and the one picked earlier can run in parallel. This is assuming
// minCompactionDepth >= 2, otherwise the second compaction has too little
// depth to pick.
//
//         _____________
//    L0.3 |a--d    g-j|      _________
//    L0.2 |       f--j|      |  r-t  |
//    L0.1 | b-d  e---j|      |       |
//    L0.0 |a--d   f--j| l--o |p-----x|
//
//    Lbase a--------i    m---------w
//
// Intra-L0: If the L0 score is high, but PickBaseCompaction() is unable to
// pick a compaction, PickIntraL0Compaction will be used to pick an intra-L0
// compaction. Similar to L0 -> Lbase compactions, we want to allow for
// multiple intra-L0 compactions and not generate wide output files that
// hinder later concurrency of L0 -> Lbase compactions. Also compactions
// that produce wide files don't reduce stack depth -- they represent wide
// rectangles in our visualization, which means many intervals have their
// depth reduced by a small amount. Typically, L0 files have non-overlapping
// sequence numbers, and sticking to that invariant would require us to
// consider intra-L0 compactions that proceed from youngest to oldest files,
// which could result in the aforementioned undesirable wide rectangle
// shape. But this non-overlapping sequence number is already relaxed in
// RocksDB -- sstables are primarily ordered by their largest sequence
// number. So we can arrange for intra-L0 compactions to capture thin and
// tall rectangles starting with the top of the stack (youngest files).
// Like the L0 -> Lbase case we order the intervals using a heuristic and
// consider each in turn. The same comment about better L0 -> Lbase heuristics
// and not being greedy applies here.
//
// Going back to a modified version of our example from earlier, let's say these
// are the base compactions in progress:
//                _______
//    L0.3  a--d  |  g-j|      _________
//    L0.2        | f--j|      |  r-t  |
//    L0.1   b-d  |e---j|      |       |
//    L0.0  a--d  | f--j| l--o |p-----x|
//
//    Lbase a---------i    m---------w
//
// Since both LBase files are compacting, the only L0 compaction that can be
// picked is an intra-L0 compaction. For this, the b--d interval has the highest
// stack depth (3), and starting with a--d in L0.3 as the seed file, we can
// iterate downward and build this compaction, assuming all files in that
// interval are not compacting and have a highest sequence number less than
// earliestUnflushedSeqNum:
//
//                _______
//    L0.3 |a--d| |  g-j|      _________
//    L0.2 |    | | f--j|      |  r-t  |
//    L0.1 | b-d| |e---j|      |       |
//    L0.0 |a--d| | f--j| l--o |p-----x|
//         ------
//    Lbase a---------i    m---------w
//

// PickBaseCompaction picks a base compaction based on the above specified
// heuristics, for the specified Lbase files and a minimum depth of overlapping
// files that can be selected for compaction. Returns nil if no compaction is
// possible.
func (s *L0Sublevels) PickBaseCompaction(
	minCompactionDepth int, baseFiles LevelSlice,
) (*L0CompactionFiles, error) {
	// For LBase compactions, we consider intervals in a greedy manner in the
	// following order:
	// - Intervals that are unlikely to be blocked due
	//   to ongoing L0 -> Lbase compactions. These are the ones with
	//   !isBaseCompacting && !intervalRangeIsBaseCompacting.
	// - Intervals that are !isBaseCompacting && intervalRangeIsBaseCompacting.
	//
	// The ordering heuristic exists just to avoid wasted work. Ideally,
	// we would consider all intervals with isBaseCompacting = false and
	// construct a compaction for it and compare the constructed compactions
	// and pick the best one. If microbenchmarks show that we can afford
	// this cost we can eliminate this heuristic.
	scoredIntervals := make([]intervalAndScore, 0, len(s.orderedIntervals))
	sublevelCount := len(s.Levels)
	for i := range s.orderedIntervals {
		interval := &s.orderedIntervals[i]
		depth := interval.fileCount - interval.compactingFileCount
		if interval.isBaseCompacting || minCompactionDepth > depth {
			continue
		}
		if interval.intervalRangeIsBaseCompacting {
			scoredIntervals = append(scoredIntervals, intervalAndScore{interval: i, score: depth})
		} else {
			// Prioritize this interval by incrementing the score by the number
			// of sublevels.
			scoredIntervals = append(scoredIntervals, intervalAndScore{interval: i, score: depth + sublevelCount})
		}
	}
	sort.Sort(intervalSorterByDecreasingScore(scoredIntervals))

	// Optimization to avoid considering different intervals that
	// are likely to choose the same seed file. Again this is just
	// to reduce wasted work.
	consideredIntervals := newBitSet(len(s.orderedIntervals))
	for _, scoredInterval := range scoredIntervals {
		interval := &s.orderedIntervals[scoredInterval.interval]
		if consideredIntervals[interval.index] {
			continue
		}

		// Pick the seed file for the interval as the file
		// in the lowest sub-level.
		f := interval.files[0]
		// Don't bother considering the intervals that are
		// covered by the seed file since they are likely
		// nearby. Note that it is possible that those intervals
		// have seed files at lower sub-levels so could be
		// viable for compaction.
		consideredIntervals.markBits(f.minIntervalIndex, f.maxIntervalIndex+1)
		if f == nil {
			return nil, errors.New("no seed file found in sublevel intervals")
		}
		if f.Compacting {
			if f.IsIntraL0Compacting {
				// If we're picking a base compaction and we came across a
				// seed file candidate that's being intra-L0 compacted, skip
				// the interval instead of erroring out.
				continue
			}
			// We chose a compaction seed file that should not be
			// compacting. Usually means the score is not accurately
			// accounting for files already compacting, or internal state is
			// inconsistent.
			return nil, errors.Errorf("file %s chosen as seed file for compaction should not be compacting", f.FileNum)
		}

		c := s.baseCompactionUsingSeed(f, interval.index, minCompactionDepth)
		if c != nil {
			// Check if the chosen compaction overlaps with any files
			// in Lbase that have Compacting = true. If that's the case,
			// this compaction cannot be chosen.
			baseIter := baseFiles.Iter()
			// An interval starting at ImmediateSuccessor(key) can never be the
			// first interval of a compaction since no file can start at that
			// interval.
			m := baseIter.SeekGE(s.cmp, s.orderedIntervals[c.minIntervalIndex].startKey.key)

			var baseCompacting bool
			for ; m != nil && !baseCompacting; m = baseIter.Next() {
				cmp := s.cmp(m.Smallest.UserKey, s.orderedIntervals[c.maxIntervalIndex+1].startKey.key)
				// Compaction is ending at exclusive bound of c.maxIntervalIndex+1
				if cmp > 0 || (cmp == 0 && !s.orderedIntervals[c.maxIntervalIndex+1].startKey.isLargest) {
					break
				}
				baseCompacting = baseCompacting || m.Compacting
			}
			if baseCompacting {
				continue
			}
			return c, nil
		}
	}
	return nil, nil
}

// Helper function for building an L0 -> Lbase compaction using a seed interval
// and seed file in that seed interval.
func (s *L0Sublevels) baseCompactionUsingSeed(
	f *FileMetadata, intervalIndex int, minCompactionDepth int,
) *L0CompactionFiles {
	c := &L0CompactionFiles{
		FilesIncluded:        newBitSet(s.levelMetadata.Len()),
		seedInterval:         intervalIndex,
		seedIntervalMinLevel: 0,
		minIntervalIndex:     f.minIntervalIndex,
		maxIntervalIndex:     f.maxIntervalIndex,
	}
	c.addFile(f)

	// The first iteration of this loop builds the compaction at the seed file's
	// sublevel. Future iterations expand on this compaction by stacking
	// more files from intervalIndex and repeating. This is an
	// optional activity so when it fails we can fallback to the last
	// successful candidate.
	var lastCandidate *L0CompactionFiles
	interval := &s.orderedIntervals[intervalIndex]
	for i := 0; i < len(interval.files); i++ {
		f2 := interval.files[i]
		sl := f2.subLevel
		c.seedIntervalStackDepthReduction++
		c.seedIntervalMaxLevel = sl
		c.addFile(f2)
		// The seed file is in the lowest sublevel in the seed interval, but it may
		// overlap with other files in even lower sublevels. For
		// correctness we need to grow our interval to include those files, and
		// capture all files in the next level that fall in this extended interval
		// and so on. This can result in a triangular shape like the following
		// where again the X axis is the key intervals and the Y axis
		// is oldest to youngest. Note that it is not necessary for
		// correctness to fill out the shape at the higher sub-levels
		// to make it more rectangular since the invariant only requires
		// that younger versions of a key not be moved to Lbase while
		// leaving behind older versions.
		//                     -
		//                    ---
		//                   -----
		// It may be better for performance to have a more rectangular
		// shape since the files being left behind will overlap with the
		// same Lbase key range as that of this compaction. But there is
		// also the danger that in trying to construct a more rectangular
		// shape we will be forced to pull in a file that is already
		// compacting. We expect extendCandidateToRectangle to eventually be called
		// on this compaction if it's chosen, at which point we would iterate
		// backward and choose those files. This logic is similar to compaction.grow
		// for non-L0 compactions.
		done := false
		for currLevel := sl - 1; currLevel >= 0; currLevel-- {
			if !s.extendFiles(currLevel, math.MaxUint64, c) {
				// Failed to extend due to ongoing compaction.
				done = true
				break
			}
		}
		if done {
			break
		}
		// Observed some compactions using > 1GB from L0 in an import
		// experiment. Very long running compactions are not great as they
		// reduce concurrency while they run, and take a while to produce
		// results, though they're sometimes unavoidable. There is a tradeoff
		// here in that adding more depth is more efficient in reducing stack
		// depth, but long running compactions reduce flexibility in what can
		// run concurrently in L0 and even Lbase -> Lbase+1. An increase more
		// than 150% in bytes since the last candidate compaction (along with a
		// total compaction size in excess of 100mb), or a total compaction
		// size beyond a hard limit of 500mb, is criteria for rejecting this
		// candidate. This lets us prefer slow growths as we add files, while
		// still having a hard limit. Note that if this is the first compaction
		// candidate to reach a stack depth reduction of minCompactionDepth or
		// higher, this candidate will be chosen regardless.
		if lastCandidate == nil {
			lastCandidate = &L0CompactionFiles{}
		} else if lastCandidate.seedIntervalStackDepthReduction >= minCompactionDepth &&
			c.fileBytes > 100<<20 &&
			(float64(c.fileBytes)/float64(lastCandidate.fileBytes) > 1.5 || c.fileBytes > 500<<20) {
			break
		}
		*lastCandidate = *c
	}
	if lastCandidate != nil && lastCandidate.seedIntervalStackDepthReduction >= minCompactionDepth {
		lastCandidate.FilesIncluded.clearAllBits()
		for _, f := range lastCandidate.Files {
			lastCandidate.FilesIncluded.markBit(f.l0Index)
		}
		return lastCandidate
	}
	return nil
}

// Expands fields in the provided L0CompactionFiles instance (cFiles) to
// include overlapping files in the specified sublevel. Returns true if the
// compaction is possible (i.e. does not conflict with any base/intra-L0
// compacting files).
func (s *L0Sublevels) extendFiles(
	sl int, earliestUnflushedSeqNum uint64, cFiles *L0CompactionFiles,
) bool {
	index := sort.Search(len(s.Levels[sl]), func(i int) bool {
		return s.Levels[sl][i].maxIntervalIndex >= cFiles.minIntervalIndex
	})
	for ; index < len(s.Levels[sl]); index++ {
		f := s.Levels[sl][index]
		if f.minIntervalIndex > cFiles.maxIntervalIndex {
			break
		}
		if f.Compacting {
			return false
		}
		// Skip over files that are newer than earliestUnflushedSeqNum. This is
		// okay because this compaction can just pretend these files are not in
		// L0 yet. These files must be in higher sublevels than any overlapping
		// files with f.LargestSeqNum < earliestUnflushedSeqNum, and the output
		// of the compaction will also go in a lower (older) sublevel than this
		// file by definition.
		if f.LargestSeqNum >= earliestUnflushedSeqNum {
			continue
		}
		cFiles.addFile(f)
	}
	return true
}

// PickIntraL0Compaction picks an intra-L0 compaction for files in this
// sublevel. This method is only called when a base compaction cannot be chosen.
// See comment above PickBaseCompaction for heuristics involved in this
// selection.
func (s *L0Sublevels) PickIntraL0Compaction(
	earliestUnflushedSeqNum uint64, minCompactionDepth int,
) (*L0CompactionFiles, error) {
	var scoredIntervals []intervalAndScore
	for i := range s.orderedIntervals {
		interval := &s.orderedIntervals[i]
		depth := interval.fileCount - interval.compactingFileCount
		if minCompactionDepth > depth {
			continue
		}
		scoredIntervals = append(scoredIntervals, intervalAndScore{interval: i, score: depth})
	}
	sort.Sort(intervalSorterByDecreasingScore(scoredIntervals))

	// Optimization to avoid considering different intervals that
	// are likely to choose the same seed file. Again this is just
	// to reduce wasted work.
	consideredIntervals := newBitSet(len(s.orderedIntervals))
	for _, scoredInterval := range scoredIntervals {
		interval := &s.orderedIntervals[scoredInterval.interval]
		if consideredIntervals[interval.index] {
			continue
		}

		var f *FileMetadata
		// Pick the seed file for the interval as the file
		// in the highest sub-level.
		stackDepthReduction := scoredInterval.score
		for i := len(interval.files) - 1; i >= 0; i-- {
			f = interval.files[i]
			if f.Compacting {
				break
			}
			consideredIntervals.markBits(f.minIntervalIndex, f.maxIntervalIndex+1)
			// Can this be the seed file? Files with newer sequence
			// numbers than earliestUnflushedSeqNum cannot be in
			// the compaction.
			if f.LargestSeqNum >= earliestUnflushedSeqNum {
				stackDepthReduction--
				if stackDepthReduction == 0 {
					break
				}
			} else {
				break
			}
		}
		if stackDepthReduction < minCompactionDepth {
			// Can't use this interval.
			continue
		}

		if f == nil {
			return nil, errors.New("no seed file found in sublevel intervals")
		}
		if f.Compacting {
			// This file could be in a concurrent intra-L0 or base compaction.
			// Try another interval.
			continue
		}

		// We have a seed file. Build a compaction off of that seed.
		c := s.intraL0CompactionUsingSeed(
			f, interval.index, earliestUnflushedSeqNum, minCompactionDepth)
		if c != nil {
			return c, nil
		}
	}
	return nil, nil
}

func (s *L0Sublevels) intraL0CompactionUsingSeed(
	f *FileMetadata, intervalIndex int, earliestUnflushedSeqNum uint64, minCompactionDepth int,
) *L0CompactionFiles {
	// We know that all the files that overlap with intervalIndex have
	// LargestSeqNum < earliestUnflushedSeqNum, but for other intervals
	// we need to exclude files >= earliestUnflushedSeqNum

	c := &L0CompactionFiles{
		FilesIncluded:           newBitSet(s.levelMetadata.Len()),
		seedInterval:            intervalIndex,
		seedIntervalMaxLevel:    len(s.Levels) - 1,
		minIntervalIndex:        f.minIntervalIndex,
		maxIntervalIndex:        f.maxIntervalIndex,
		isIntraL0:               true,
		earliestUnflushedSeqNum: earliestUnflushedSeqNum,
	}
	c.addFile(f)

	var lastCandidate *L0CompactionFiles
	interval := &s.orderedIntervals[intervalIndex]
	slIndex := len(interval.files) - 1
	for {
		if interval.files[slIndex] == f {
			break
		}
		slIndex--
	}
	// The first iteration of this loop produces an intra-L0 compaction at the
	// seed level. Iterations after that optionally add to the compaction by
	// stacking more files from intervalIndex and repeating. This is an
	// optional activity so when it fails we can fallback to the last
	// successful candidate. The code stops adding when it can't add more, or
	// when fileBytes grows too large.
	for ; slIndex >= 0; slIndex-- {
		f2 := interval.files[slIndex]
		sl := f2.subLevel
		if f2.Compacting {
			break
		}
		c.seedIntervalStackDepthReduction++
		c.seedIntervalMinLevel = sl
		c.addFile(f2)
		// The seed file captures all files in the higher level that fall in the
		// range of intervals. That may extend the range of intervals so for
		// correctness we need to capture all files in the next higher level that
		// fall in this extended interval and so on. This can result in an
		// inverted triangular shape like the following where again the X axis is the
		// key intervals and the Y axis is oldest to youngest. Note that it is not
		// necessary for correctness to fill out the shape at lower sub-levels to
		// make it more rectangular since the invariant only requires that if we
		// move an older seqnum for key k into a file that has a higher seqnum, we
		// also move all younger seqnums for that key k into that file.
		//                  -----
		//                   ---
		//                    -
		//
		// It may be better for performance to have a more rectangular shape since
		// it will reduce the stack depth for more intervals. But there is also
		// the danger that in explicitly trying to construct a more rectangular
		// shape we will be forced to pull in a file that is already compacting.
		// We assume that the performance concern is not a practical issue.
		done := false
		for currLevel := sl + 1; currLevel < len(s.Levels); currLevel++ {
			if !s.extendFiles(currLevel, earliestUnflushedSeqNum, c) {
				// Failed to extend due to ongoing compaction.
				done = true
				break
			}
		}
		if done {
			break
		}
		if lastCandidate == nil {
			lastCandidate = &L0CompactionFiles{}
		} else if lastCandidate.seedIntervalStackDepthReduction >= minCompactionDepth &&
			c.fileBytes > 100<<20 &&
			(float64(c.fileBytes)/float64(lastCandidate.fileBytes) > 1.5 || c.fileBytes > 500<<20) {
			break
		}
		*lastCandidate = *c
	}
	if lastCandidate != nil && lastCandidate.seedIntervalStackDepthReduction >= minCompactionDepth {
		lastCandidate.FilesIncluded.clearAllBits()
		for _, f := range lastCandidate.Files {
			lastCandidate.FilesIncluded.markBit(f.l0Index)
		}
		s.extendCandidateToRectangle(
			lastCandidate.minIntervalIndex, lastCandidate.maxIntervalIndex, lastCandidate, false)
		return lastCandidate
	}
	return nil
}

// ExtendL0ForBaseCompactionTo extends the specified base compaction candidate
// L0CompactionFiles to optionally cover more files in L0 without "touching"
// any of the passed-in keys (i.e. the smallest/largest bounds are exclusive),
// as including any user keys for those internal keys
// could require choosing more files in LBase which is undesirable. Unbounded
// start/end keys are indicated by passing in the InvalidInternalKey.
func (s *L0Sublevels) ExtendL0ForBaseCompactionTo(
	smallest, largest InternalKey, candidate *L0CompactionFiles,
) bool {
	firstIntervalIndex := 0
	lastIntervalIndex := len(s.orderedIntervals) - 1
	if smallest.Kind() != base.InternalKeyKindInvalid {
		if smallest.Trailer == base.InternalKeyRangeDeleteSentinel {
			// Starting at smallest.UserKey == interval.startKey is okay.
			firstIntervalIndex = sort.Search(len(s.orderedIntervals), func(i int) bool {
				return s.cmp(smallest.UserKey, s.orderedIntervals[i].startKey.key) <= 0
			})
		} else {
			firstIntervalIndex = sort.Search(len(s.orderedIntervals), func(i int) bool {
				// Need to start at >= smallest since if we widen too much we may miss
				// an Lbase file that overlaps with an L0 file that will get picked in
				// this widening, which would be bad. This interval will not start with
				// an immediate successor key.
				return s.cmp(smallest.UserKey, s.orderedIntervals[i].startKey.key) < 0
			})
		}
	}
	if largest.Kind() != base.InternalKeyKindInvalid {
		// First interval that starts at or beyond the largest. This interval will not
		// start with an immediate successor key.
		lastIntervalIndex = sort.Search(len(s.orderedIntervals), func(i int) bool {
			return s.cmp(largest.UserKey, s.orderedIntervals[i].startKey.key) <= 0
		})
		// Right now, lastIntervalIndex has a startKey that extends beyond largest.
		// The previous interval, by definition, has an end key higher than largest.
		// Iterate back twice to get the last interval that's completely within
		// (smallest, largest). Except in the case where we went past the end of the
		// list; in that case, the last interval to include is the very last
		// interval in the list.
		if lastIntervalIndex < len(s.orderedIntervals) {
			lastIntervalIndex--
		}
		lastIntervalIndex--
	}
	if lastIntervalIndex < firstIntervalIndex {
		return false
	}
	return s.extendCandidateToRectangle(firstIntervalIndex, lastIntervalIndex, candidate, true)
}

// Best-effort attempt to make the compaction include more files in the
// rectangle defined by [minIntervalIndex, maxIntervalIndex] on the X axis and
// bounded on the Y axis by seedIntervalMinLevel and seedIntervalMaxLevel.
//
// This is strictly an optional extension; at any point where we can't feasibly
// add more files, the sublevel iteration can be halted early and candidate will
// still be a correct compaction candidate.
//
// Consider this scenario (original candidate is inside the rectangle), with
// isBase = true and interval bounds a-j (from the union of base file bounds and
// that of compaction candidate):
//               _______
//    L0.3  a--d |  g-j|
//    L0.2       | f--j|         r-t
//    L0.1   b-d |e---j|
//    L0.0  a--d | f--j| l--o  p-----x
//
//    Lbase a--------i    m---------w
//
// This method will iterate from the bottom up. At L0.0, it will add a--d since
// it's in the bounds, then add b-d, then a--d, and so on, to produce this:
//
//         _____________
//    L0.3 |a--d    g-j|
//    L0.2 |       f--j|         r-t
//    L0.1 | b-d  e---j|
//    L0.0 |a--d   f--j| l--o  p-----x
//
//    Lbase a-------i     m---------w
//
// Let's assume that, instead of a--d in the top sublevel, we had 3 files, a-b,
// bb-c, and cc-d, of which bb-c is compacting. Let's also add another sublevel
// L0.4 with some files, all of which aren't compacting:
//
//    L0.4  a------c ca--d _______
//    L0.3  a-b bb-c  cc-d |  g-j|
//    L0.2                 | f--j|         r-t
//    L0.1    b----------d |e---j|
//    L0.0  a------------d | f--j| l--o  p-----x
//
//    Lbase a------------------i    m---------w
//
// This method then needs to choose between the left side of L0.3 bb-c
// (i.e. a-b), or the right side (i.e. cc-d and g-j) for inclusion in this
// compaction. Since the right side has more files as well as one file that has
// already been picked, it gets chosen at that sublevel, resulting in this
// intermediate compaction:
//
//    L0.4  a------c ca--d
//                  ______________
//    L0.3  a-b bb-c| cc-d    g-j|
//    L0.2 _________|        f--j|         r-t
//    L0.1 |  b----------d  e---j|
//    L0.0 |a------------d   f--j| l--o  p-----x
//
//    Lbase a------------------i    m---------w
//
// Since bb-c had to be excluded at L0.3, the interval bounds for L0.4 are
// actually ca-j, since ca is the next interval start key after the end interval
// of bb-c. This would result in only ca-d being chosen at that sublevel, even
// though a--c is also not compacting. This is the final result:
//
//                  ______________
//    L0.4  a------c|ca--d       |
//    L0.3  a-b bb-c| cc-d    g-j|
//    L0.2 _________|        f--j|         r-t
//    L0.1 |  b----------d  e---j|
//    L0.0 |a------------d   f--j| l--o  p-----x
//
//    Lbase a------------------i    m---------w
//
// TODO(bilal): Add more targeted tests for this method, through
// ExtendL0ForBaseCompactionTo and intraL0CompactionUsingSeed.
func (s *L0Sublevels) extendCandidateToRectangle(
	minIntervalIndex int, maxIntervalIndex int, candidate *L0CompactionFiles, isBase bool,
) bool {
	candidate.preExtensionMinInterval = candidate.minIntervalIndex
	candidate.preExtensionMaxInterval = candidate.maxIntervalIndex
	// Extend {min,max}IntervalIndex to include all of the candidate's current
	// bounds.
	if minIntervalIndex > candidate.minIntervalIndex {
		minIntervalIndex = candidate.minIntervalIndex
	}
	if maxIntervalIndex < candidate.maxIntervalIndex {
		maxIntervalIndex = candidate.maxIntervalIndex
	}
	var startLevel, increment, endLevel int
	if isBase {
		startLevel = 0
		increment = +1
		// seedIntervalMaxLevel is inclusive, while endLevel is exclusive.
		endLevel = candidate.seedIntervalMaxLevel + 1
	} else {
		startLevel = len(s.Levels) - 1
		increment = -1
		// seedIntervalMinLevel is inclusive, while endLevel is exclusive.
		endLevel = candidate.seedIntervalMinLevel - 1
	}
	// Stats for files.
	addedCount := 0
	// Iterate from the oldest sub-level for L0 -> Lbase and youngest
	// sub-level for intra-L0. The idea here is that anything that can't
	// be included from that level constrains what can be included from
	// the next level. This change in constraint is directly incorporated
	// into minIntervalIndex, maxIntervalIndex.
	for sl := startLevel; sl != endLevel; sl += increment {
		files := s.Levels[sl]
		// Find the first file that overlaps with minIntervalIndex.
		index := sort.Search(len(files), func(i int) bool {
			return minIntervalIndex <= files[i].maxIntervalIndex
		})
		// Track the files that are fully within the current constraint
		// of [minIntervalIndex, maxIntervalIndex].
		firstIndex := -1
		lastIndex := -1
		for ; index < len(files); index++ {
			f := files[index]
			if f.minIntervalIndex > maxIntervalIndex {
				break
			}
			include := true
			// Extends out on the left so can't be included. This narrows
			// what we can included in the next level.
			if f.minIntervalIndex < minIntervalIndex {
				include = false
				minIntervalIndex = f.maxIntervalIndex + 1
			}
			// Extends out on the right so can't be included.
			if f.maxIntervalIndex > maxIntervalIndex {
				include = false
				maxIntervalIndex = f.minIntervalIndex - 1
			}
			if !include {
				continue
			}
			if firstIndex == -1 {
				firstIndex = index
			}
			lastIndex = index
		}
		if minIntervalIndex > maxIntervalIndex {
			// We excluded files that prevent continuation.
			break
		}
		if firstIndex < 0 {
			// No files to add in this sub-level.
			continue
		}
		// We have the files in [firstIndex, lastIndex] as potential for
		// inclusion. Some of these may already have been picked. Some
		// of them may be already compacting. The latter is tricky since
		// we have to decide whether to contract minIntervalIndex or
		// maxIntervalIndex when we encounter an already compacting file.
		// We pick the longest sequence between firstIndex
		// and lastIndex of non-compacting files -- this is represented by
		// [candidateNonCompactingFirst, candidateNonCompactingLast].
		nonCompactingFirst := -1
		currentRunHasAlreadyPickedFiles := false
		candidateNonCompactingFirst := -1
		candidateNonCompactingLast := -1
		candidateHasAlreadyPickedFiles := false
		for index = firstIndex; index <= lastIndex; index++ {
			f := files[index]
			if f.Compacting {
				if nonCompactingFirst != -1 {
					last := index - 1
					// Prioritize runs of consecutive non-compacting files that
					// have files that have already been picked. That is to say,
					// if candidateHasAlreadyPickedFiles == true, we stick with
					// it, and if currentRunHasAlreadyPickedfiles == true, we
					// pick that run even if it contains fewer files than the
					// previous candidate.
					if !candidateHasAlreadyPickedFiles && (candidateNonCompactingFirst == -1 ||
						currentRunHasAlreadyPickedFiles ||
						(last-nonCompactingFirst) > (candidateNonCompactingLast-candidateNonCompactingFirst)) {
						candidateNonCompactingFirst = nonCompactingFirst
						candidateNonCompactingLast = last
						candidateHasAlreadyPickedFiles = currentRunHasAlreadyPickedFiles
					}
				}
				nonCompactingFirst = -1
				currentRunHasAlreadyPickedFiles = false
				continue
			}
			if nonCompactingFirst == -1 {
				nonCompactingFirst = index
			}
			if candidate.FilesIncluded[f.l0Index] {
				currentRunHasAlreadyPickedFiles = true
			}
		}
		// Logic duplicated from inside the for loop above.
		if nonCompactingFirst != -1 {
			last := index - 1
			if !candidateHasAlreadyPickedFiles && (candidateNonCompactingFirst == -1 ||
				currentRunHasAlreadyPickedFiles ||
				(last-nonCompactingFirst) > (candidateNonCompactingLast-candidateNonCompactingFirst)) {
				candidateNonCompactingFirst = nonCompactingFirst
				candidateNonCompactingLast = last
			}
		}
		if candidateNonCompactingFirst == -1 {
			// All files are compacting. There will be gaps that we could exploit
			// to continue, but don't bother.
			break
		}
		// May need to shrink [minIntervalIndex, maxIntervalIndex] for the next level.
		if candidateNonCompactingFirst > firstIndex {
			minIntervalIndex = files[candidateNonCompactingFirst-1].maxIntervalIndex + 1
		}
		if candidateNonCompactingLast < lastIndex {
			maxIntervalIndex = files[candidateNonCompactingLast+1].minIntervalIndex - 1
		}
		for index := candidateNonCompactingFirst; index <= candidateNonCompactingLast; index++ {
			f := files[index]
			if f.Compacting {
				// TODO(bilal): Do a logger.Fatalf instead of a panic, for
				// cleaner unwinding and error messages.
				panic(fmt.Sprintf("expected %s to not be compacting", f.FileNum))
			}
			if candidate.isIntraL0 && f.LargestSeqNum >= candidate.earliestUnflushedSeqNum {
				continue
			}
			if !candidate.FilesIncluded[f.l0Index] {
				addedCount++
				candidate.addFile(f)
			}
		}
	}
	return addedCount > 0
}
