// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package storage

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"github.com/uber-go/tally"

	"github.com/m3db/m3/src/dbnode/namespace"
	"github.com/m3db/m3/src/dbnode/persist"
	"github.com/m3db/m3/src/dbnode/persist/fs/commitlog"
	"github.com/m3db/m3/src/dbnode/retention"
	"github.com/m3db/m3/src/x/ident"
	xtest "github.com/m3db/m3/src/x/test"
	xtime "github.com/m3db/m3/src/x/time"
)

var testCommitlogFile = persist.CommitLogFile{
	FilePath: "/var/lib/m3db/commitlogs/commitlog-0-0.db",
	Index:    0,
}

func newMultipleFlushManagerNeedsFlush(t *testing.T, ctrl *gomock.Controller) (
	*flushManager,
	*MockdatabaseNamespace,
	*MockdatabaseNamespace,
	*commitlog.MockCommitLog,
) {
	options := namespace.NewOptions()
	namespace := NewMockdatabaseNamespace(ctrl)
	namespace.EXPECT().Options().Return(options).AnyTimes()
	namespace.EXPECT().ID().Return(defaultTestNs1ID).AnyTimes()
	otherNamespace := NewMockdatabaseNamespace(ctrl)
	otherNamespace.EXPECT().Options().Return(options).AnyTimes()
	otherNamespace.EXPECT().ID().Return(ident.StringID("someString")).AnyTimes()

	db := newMockdatabase(ctrl, namespace, otherNamespace)

	cl := commitlog.NewMockCommitLog(ctrl)
	cl.EXPECT().RotateLogs().Return(testCommitlogFile, nil).AnyTimes()

	fm := newFlushManager(db, cl, tally.NoopScope).(*flushManager)

	return fm, namespace, otherNamespace, cl
}

func TestFlushManagerFlushAlreadyInProgress(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	var (
		// Channels used to coordinate flushing / snapshotting
		startCh = make(chan struct{}, 1)
		doneCh  = make(chan struct{}, 1)
	)
	defer func() {
		close(startCh)
		close(doneCh)
	}()

	var (
		mockPersistManager  = persist.NewMockManager(ctrl)
		mockFlushPerist     = persist.NewMockFlushPreparer(ctrl)
		mockSnapshotPersist = persist.NewMockSnapshotPreparer(ctrl)
	)

	mockFlushPerist.EXPECT().DoneFlush().Return(nil).AnyTimes()
	mockPersistManager.EXPECT().StartFlushPersist().Do(func() {
		startCh <- struct{}{}
		<-doneCh
	}).Return(mockFlushPerist, nil).AnyTimes()

	mockSnapshotPersist.EXPECT().DoneSnapshot(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockPersistManager.EXPECT().StartSnapshotPersist(gomock.Any()).Do(func(_ interface{}) {
		startCh <- struct{}{}
		<-doneCh
	}).Return(mockSnapshotPersist, nil).AnyTimes()

	mockIndexFlusher := persist.NewMockIndexFlush(ctrl)
	mockIndexFlusher.EXPECT().DoneIndex().Return(nil).AnyTimes()
	mockPersistManager.EXPECT().StartIndexPersist().Return(mockIndexFlusher, nil).AnyTimes()

	testOpts := DefaultTestOptions().SetPersistManager(mockPersistManager)
	db := newMockdatabase(ctrl)
	db.EXPECT().Options().Return(testOpts).AnyTimes()
	db.EXPECT().OwnedNamespaces().Return(nil, nil).AnyTimes()

	cl := commitlog.NewMockCommitLog(ctrl)
	cl.EXPECT().RotateLogs().Return(testCommitlogFile, nil).AnyTimes()

	fm := newFlushManager(db, cl, tally.NoopScope).(*flushManager)
	fm.pm = mockPersistManager

	now := xtime.UnixNano(0)
	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1 should successfully flush.
	go func() {
		defer wg.Done()
		require.NoError(t, fm.Flush(now))
	}()

	// Goroutine 2 should indicate already flushing.
	go func() {
		defer wg.Done()

		// Wait until we start the flushing process.
		<-startCh

		// Ensure it doesn't allow a parallel flush.
		require.Equal(t, errFlushOperationsInProgress, fm.Flush(now))

		// Allow the flush to finish.
		doneCh <- struct{}{}

		// Allow the snapshot to begin and finish.
		<-startCh

		// Ensure it doesn't allow a parallel flush.
		require.Equal(t, errFlushOperationsInProgress, fm.Flush(now))

		doneCh <- struct{}{}
	}()

	wg.Wait()
}

// TestFlushManagerFlushDoneFlushError makes sure that flush errors do not
// impact snapshotting or index operations.
func TestFlushManagerFlushDoneFlushError(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	var (
		fakeErr             = errors.New("fake error while marking flush done")
		mockPersistManager  = persist.NewMockManager(ctrl)
		mockFlushPersist    = persist.NewMockFlushPreparer(ctrl)
		mockSnapshotPersist = persist.NewMockSnapshotPreparer(ctrl)
	)

	mockFlushPersist.EXPECT().DoneFlush().Return(fakeErr)
	mockPersistManager.EXPECT().StartFlushPersist().Return(mockFlushPersist, nil)

	mockSnapshotPersist.EXPECT().DoneSnapshot(gomock.Any(), testCommitlogFile).Return(nil)
	mockPersistManager.EXPECT().StartSnapshotPersist(gomock.Any()).Return(mockSnapshotPersist, nil)

	mockIndexFlusher := persist.NewMockIndexFlush(ctrl)
	mockIndexFlusher.EXPECT().DoneIndex().Return(nil)
	mockPersistManager.EXPECT().StartIndexPersist().Return(mockIndexFlusher, nil)

	testOpts := DefaultTestOptions().SetPersistManager(mockPersistManager)
	db := newMockdatabase(ctrl)
	db.EXPECT().Options().Return(testOpts).AnyTimes()
	db.EXPECT().OwnedNamespaces().Return(nil, nil)

	cl := commitlog.NewMockCommitLog(ctrl)
	cl.EXPECT().RotateLogs().Return(testCommitlogFile, nil).AnyTimes()

	fm := newFlushManager(db, cl, tally.NoopScope).(*flushManager)
	fm.pm = mockPersistManager

	now := xtime.UnixNano(0)
	require.EqualError(t, fakeErr, fm.Flush(now).Error())
}

// TestFlushManagerNamespaceFlushTimesErr makes sure that namespaceFlushTimes errors do
// not leave the persist manager in an invalid state.
func TestFlushManagerNamespaceFlushTimesErr(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	var (
		fakeErr             = errors.New("some-err")
		mockPersistManager  = persist.NewMockManager(ctrl)
		mockFlushPersist    = persist.NewMockFlushPreparer(ctrl)
		mockSnapshotPersist = persist.NewMockSnapshotPreparer(ctrl)
	)

	// Make sure DoneFlush is called despite encountering an error, once for snapshot and once for warm flush.
	mockFlushPersist.EXPECT().DoneFlush().Return(nil)
	mockPersistManager.EXPECT().StartFlushPersist().Return(mockFlushPersist, nil)

	mockSnapshotPersist.EXPECT().DoneSnapshot(gomock.Any(), testCommitlogFile).Return(nil)
	mockPersistManager.EXPECT().StartSnapshotPersist(gomock.Any()).Return(mockSnapshotPersist, nil)

	mockIndexFlusher := persist.NewMockIndexFlush(ctrl)
	mockIndexFlusher.EXPECT().DoneIndex().Return(nil)
	mockPersistManager.EXPECT().StartIndexPersist().Return(mockIndexFlusher, nil)

	testOpts := DefaultTestOptions().SetPersistManager(mockPersistManager)
	db := newMockdatabase(ctrl)
	db.EXPECT().Options().Return(testOpts).AnyTimes()

	nsOpts := defaultTestNs1Opts.SetIndexOptions(namespace.NewIndexOptions().SetEnabled(false))
	ns := NewMockdatabaseNamespace(ctrl)
	ns.EXPECT().Options().Return(nsOpts).AnyTimes()
	ns.EXPECT().ID().Return(defaultTestNs1ID).AnyTimes()
	ns.EXPECT().NeedsFlush(gomock.Any(), gomock.Any()).Return(false, fakeErr).AnyTimes()
	ns.EXPECT().Snapshot(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	db.EXPECT().OwnedNamespaces().Return([]databaseNamespace{ns}, nil)

	cl := commitlog.NewMockCommitLog(ctrl)
	cl.EXPECT().RotateLogs().Return(testCommitlogFile, nil).AnyTimes()

	fm := newFlushManager(db, cl, tally.NoopScope).(*flushManager)
	fm.pm = mockPersistManager

	now := xtime.UnixNano(0)
	require.True(t, strings.Contains(fm.Flush(now).Error(), fakeErr.Error()))
}

// TestFlushManagerFlushDoneSnapshotError makes sure that snapshot errors do not
// impact flushing or index operations.
func TestFlushManagerFlushDoneSnapshotError(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	var (
		fakeErr             = errors.New("fake error while marking snapshot done")
		mockPersistManager  = persist.NewMockManager(ctrl)
		mockFlushPersist    = persist.NewMockFlushPreparer(ctrl)
		mockSnapshotPersist = persist.NewMockSnapshotPreparer(ctrl)
	)

	mockFlushPersist.EXPECT().DoneFlush().Return(nil)
	mockPersistManager.EXPECT().StartFlushPersist().Return(mockFlushPersist, nil)

	mockSnapshotPersist.EXPECT().DoneSnapshot(gomock.Any(), testCommitlogFile).Return(fakeErr)
	mockPersistManager.EXPECT().StartSnapshotPersist(gomock.Any()).Return(mockSnapshotPersist, nil)

	mockIndexFlusher := persist.NewMockIndexFlush(ctrl)
	mockIndexFlusher.EXPECT().DoneIndex().Return(nil)
	mockPersistManager.EXPECT().StartIndexPersist().Return(mockIndexFlusher, nil)

	testOpts := DefaultTestOptions().SetPersistManager(mockPersistManager)
	db := newMockdatabase(ctrl)
	db.EXPECT().Options().Return(testOpts).AnyTimes()
	db.EXPECT().OwnedNamespaces().Return(nil, nil)

	cl := commitlog.NewMockCommitLog(ctrl)
	cl.EXPECT().RotateLogs().Return(testCommitlogFile, nil).AnyTimes()

	fm := newFlushManager(db, cl, tally.NoopScope).(*flushManager)
	fm.pm = mockPersistManager

	now := xtime.UnixNano(0)
	require.EqualError(t, fakeErr, fm.Flush(now).Error())
}

func TestFlushManagerFlushDoneIndexError(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	var (
		mockFlushPersist    = persist.NewMockFlushPreparer(ctrl)
		mockSnapshotPersist = persist.NewMockSnapshotPreparer(ctrl)
		mockPersistManager  = persist.NewMockManager(ctrl)
	)

	mockFlushPersist.EXPECT().DoneFlush().Return(nil)
	mockPersistManager.EXPECT().StartFlushPersist().Return(mockFlushPersist, nil)

	mockSnapshotPersist.EXPECT().DoneSnapshot(gomock.Any(), testCommitlogFile).Return(nil)
	mockPersistManager.EXPECT().StartSnapshotPersist(gomock.Any()).Return(mockSnapshotPersist, nil)

	fakeErr := errors.New("fake error while marking flush done")
	mockIndexFlusher := persist.NewMockIndexFlush(ctrl)
	mockIndexFlusher.EXPECT().DoneIndex().Return(fakeErr)
	mockPersistManager.EXPECT().StartIndexPersist().Return(mockIndexFlusher, nil)

	testOpts := DefaultTestOptions().SetPersistManager(mockPersistManager)
	db := newMockdatabase(ctrl)
	db.EXPECT().Options().Return(testOpts).AnyTimes()
	db.EXPECT().OwnedNamespaces().Return(nil, nil)

	cl := commitlog.NewMockCommitLog(ctrl)
	cl.EXPECT().RotateLogs().Return(testCommitlogFile, nil).AnyTimes()

	fm := newFlushManager(db, cl, tally.NoopScope).(*flushManager)
	fm.pm = mockPersistManager

	now := xtime.UnixNano(0)
	require.EqualError(t, fakeErr, fm.Flush(now).Error())
}

func TestFlushManagerSkipNamespaceIndexingDisabled(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	nsOpts := defaultTestNs1Opts.SetIndexOptions(namespace.NewIndexOptions().SetEnabled(false))
	s1 := NewMockdatabaseShard(ctrl)
	s2 := NewMockdatabaseShard(ctrl)
	ns := NewMockdatabaseNamespace(ctrl)
	ns.EXPECT().Options().Return(nsOpts).AnyTimes()
	ns.EXPECT().ID().Return(defaultTestNs1ID).AnyTimes()
	ns.EXPECT().NeedsFlush(gomock.Any(), gomock.Any()).Return(true, nil).AnyTimes()
	ns.EXPECT().WarmFlush(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	ns.EXPECT().Snapshot(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	s1.EXPECT().ID().Return(uint32(1)).AnyTimes()
	s2.EXPECT().ID().Return(uint32(2)).AnyTimes()

	var (
		mockFlushPersist    = persist.NewMockFlushPreparer(ctrl)
		mockSnapshotPersist = persist.NewMockSnapshotPreparer(ctrl)
		mockPersistManager  = persist.NewMockManager(ctrl)
	)

	mockFlushPersist.EXPECT().DoneFlush().Return(nil)
	mockPersistManager.EXPECT().StartFlushPersist().Return(mockFlushPersist, nil)

	mockSnapshotPersist.EXPECT().DoneSnapshot(gomock.Any(), testCommitlogFile).Return(nil)
	mockPersistManager.EXPECT().StartSnapshotPersist(gomock.Any()).Return(mockSnapshotPersist, nil)

	mockIndexFlusher := persist.NewMockIndexFlush(ctrl)
	mockIndexFlusher.EXPECT().DoneIndex().Return(nil)
	mockPersistManager.EXPECT().StartIndexPersist().Return(mockIndexFlusher, nil)

	testOpts := DefaultTestOptions().SetPersistManager(mockPersistManager)
	db := newMockdatabase(ctrl)
	db.EXPECT().Options().Return(testOpts).AnyTimes()
	db.EXPECT().OwnedNamespaces().Return([]databaseNamespace{ns}, nil)

	cl := commitlog.NewMockCommitLog(ctrl)
	cl.EXPECT().RotateLogs().Return(testCommitlogFile, nil).AnyTimes()

	fm := newFlushManager(db, cl, tally.NoopScope).(*flushManager)
	fm.pm = mockPersistManager

	now := xtime.UnixNano(0)
	require.NoError(t, fm.Flush(now))
}

func TestFlushManagerNamespaceIndexingEnabled(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	nsOpts := defaultTestNs1Opts.SetIndexOptions(namespace.NewIndexOptions().SetEnabled(true))
	s1 := NewMockdatabaseShard(ctrl)
	s2 := NewMockdatabaseShard(ctrl)
	ns := NewMockdatabaseNamespace(ctrl)
	ns.EXPECT().Options().Return(nsOpts).AnyTimes()
	ns.EXPECT().ID().Return(defaultTestNs1ID).AnyTimes()
	ns.EXPECT().NeedsFlush(gomock.Any(), gomock.Any()).Return(true, nil).AnyTimes()
	s1.EXPECT().ID().Return(uint32(1)).AnyTimes()
	s2.EXPECT().ID().Return(uint32(2)).AnyTimes()

	// Validate that the flush state is marked as successful only AFTER all prequisite steps have been run.
	// Order is important to avoid any edge case where data is GCed from memory without all flushing operations
	// being completed.
	gomock.InOrder(
		ns.EXPECT().WarmFlush(gomock.Any(), gomock.Any()).Return(nil).AnyTimes(),
		ns.EXPECT().Snapshot(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes(),
		ns.EXPECT().FlushIndex(gomock.Any()).Return(nil),
	)

	var (
		mockFlushPersist    = persist.NewMockFlushPreparer(ctrl)
		mockSnapshotPersist = persist.NewMockSnapshotPreparer(ctrl)
		mockPersistManager  = persist.NewMockManager(ctrl)
	)

	mockFlushPersist.EXPECT().DoneFlush().Return(nil)
	mockPersistManager.EXPECT().StartFlushPersist().Return(mockFlushPersist, nil)

	mockSnapshotPersist.EXPECT().DoneSnapshot(gomock.Any(), testCommitlogFile).Return(nil)
	mockPersistManager.EXPECT().StartSnapshotPersist(gomock.Any()).Return(mockSnapshotPersist, nil)

	mockIndexFlusher := persist.NewMockIndexFlush(ctrl)
	mockIndexFlusher.EXPECT().DoneIndex().Return(nil)
	mockPersistManager.EXPECT().StartIndexPersist().Return(mockIndexFlusher, nil)

	testOpts := DefaultTestOptions().SetPersistManager(mockPersistManager)
	db := newMockdatabase(ctrl)
	db.EXPECT().Options().Return(testOpts).AnyTimes()
	db.EXPECT().OwnedNamespaces().Return([]databaseNamespace{ns}, nil)

	cl := commitlog.NewMockCommitLog(ctrl)
	cl.EXPECT().RotateLogs().Return(testCommitlogFile, nil).AnyTimes()

	fm := newFlushManager(db, cl, tally.NoopScope).(*flushManager)
	fm.pm = mockPersistManager

	now := xtime.UnixNano(0)
	require.NoError(t, fm.Flush(now))
}

func TestFlushManagerFlushTimeStart(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	inputs := []struct {
		ts       xtime.UnixNano
		expected xtime.UnixNano
	}{
		{
			ts:       xtime.FromSeconds(86400 * 2),
			expected: xtime.UnixNano(0),
		},
		{
			ts:       xtime.FromSeconds(86400*2 + 7200),
			expected: xtime.FromSeconds(7200),
		},
		{
			ts:       xtime.FromSeconds(86400*2 + 10800),
			expected: xtime.FromSeconds(7200),
		},
	}

	fm, _, _, _ := newMultipleFlushManagerNeedsFlush(t, ctrl)
	for _, input := range inputs {
		start, _ := fm.flushRange(defaultTestRetentionOpts, input.ts)
		require.Equal(t, input.expected, start)
	}
}

func TestFlushManagerFlushTimeEnd(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	inputs := []struct {
		ts       xtime.UnixNano
		expected xtime.UnixNano
	}{
		{
			ts:       xtime.FromSeconds(7800),
			expected: xtime.UnixNano(0),
		},
		{
			ts:       xtime.FromSeconds(8000),
			expected: xtime.UnixNano(0),
		},
		{
			ts:       xtime.FromSeconds(15200),
			expected: xtime.FromSeconds(7200),
		},
	}

	fm, _, _, _ := newMultipleFlushManagerNeedsFlush(t, ctrl)
	for _, input := range inputs {
		_, end := fm.flushRange(defaultTestRetentionOpts, input.ts)
		require.Equal(t, input.expected, end)
	}
}

func TestFlushManagerNamespaceFlushTimesNoNeedFlush(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	fm, ns1, _, _ := newMultipleFlushManagerNeedsFlush(t, ctrl)
	now := xtime.Now()

	ns1.EXPECT().NeedsFlush(gomock.Any(), gomock.Any()).Return(false, nil).AnyTimes()
	flushTimes, err := fm.namespaceFlushTimes(ns1, now)
	require.NoError(t, err)
	require.Empty(t, flushTimes)
}

func TestFlushManagerNamespaceFlushTimesError(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	fm, ns1, _, _ := newMultipleFlushManagerNeedsFlush(t, ctrl)
	now := xtime.Now()

	ns1.EXPECT().
		NeedsFlush(gomock.Any(), gomock.Any()).
		Return(false, errors.New("an error")).
		AnyTimes()
	_, err := fm.namespaceFlushTimes(ns1, now)
	require.Error(t, err)
}

func TestFlushManagerNamespaceFlushTimesAllNeedFlush(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	fm, ns1, _, _ := newMultipleFlushManagerNeedsFlush(t, ctrl)
	now := xtime.Now()

	ns1.EXPECT().NeedsFlush(gomock.Any(), gomock.Any()).Return(true, nil).AnyTimes()
	times, err := fm.namespaceFlushTimes(ns1, now)
	require.NoError(t, err)
	sort.Sort(timesInOrder(times))

	blockSize := ns1.Options().RetentionOptions().BlockSize()
	start := retention.FlushTimeStart(ns1.Options().RetentionOptions(), now)
	end := retention.FlushTimeEnd(ns1.Options().RetentionOptions(), now)

	require.Equal(t, numIntervals(start, end, blockSize), len(times))
	for i, ti := range times {
		require.Equal(t, start.Add(time.Duration(i)*blockSize), ti)
	}
}

func TestFlushManagerNamespaceFlushTimesSomeNeedFlush(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	fm, ns1, _, _ := newMultipleFlushManagerNeedsFlush(t, ctrl)
	now := xtime.Now()

	blockSize := ns1.Options().RetentionOptions().BlockSize()
	start := retention.FlushTimeStart(ns1.Options().RetentionOptions(), now)
	end := retention.FlushTimeEnd(ns1.Options().RetentionOptions(), now)
	num := numIntervals(start, end, blockSize)

	var expectedTimes []xtime.UnixNano
	for i := 0; i < num; i++ {
		st := start.Add(time.Duration(i) * blockSize)

		// skip 1/3 of input
		if i%3 == 0 {
			ns1.EXPECT().NeedsFlush(st, st).Return(false, nil)
			continue
		}

		ns1.EXPECT().NeedsFlush(st, st).Return(true, nil)
		expectedTimes = append(expectedTimes, st)
	}

	times, err := fm.namespaceFlushTimes(ns1, now)
	require.NoError(t, err)
	require.NotEmpty(t, times)
	sort.Sort(timesInOrder(times))
	require.Equal(t, expectedTimes, times)
}

func TestFlushManagerFlushSnapshot(t *testing.T) {
	ctrl := xtest.NewController(t)
	defer ctrl.Finish()

	fm, ns1, ns2, _ := newMultipleFlushManagerNeedsFlush(t, ctrl)
	now := xtime.Now()

	for _, ns := range []*MockdatabaseNamespace{ns1, ns2} {
		rOpts := ns.Options().RetentionOptions()
		blockSize := rOpts.BlockSize()
		bufferFuture := rOpts.BufferFuture()

		start := retention.FlushTimeStart(ns.Options().RetentionOptions(), now)
		flushEnd := retention.FlushTimeEnd(ns.Options().RetentionOptions(), now)
		num := numIntervals(start, flushEnd, blockSize)

		for i := 0; i < num; i++ {
			st := start.Add(time.Duration(i) * blockSize)
			ns.EXPECT().NeedsFlush(st, st).Return(false, nil)
		}

		var (
			snapshotEnd    = now.Add(bufferFuture).Truncate(blockSize)
			snapshotBlocks []xtime.UnixNano
		)
		num = numIntervals(start, snapshotEnd, blockSize)
		for i := num - 1; i >= 0; i-- {
			snapshotBlocks = append(snapshotBlocks, start.Add(time.Duration(i)*blockSize))
		}
		ns.EXPECT().Snapshot(snapshotBlocks, now, gomock.Any())
	}

	require.NoError(t, fm.Flush(now))

	lastSuccessfulSnapshot, ok := fm.LastSuccessfulSnapshotStartTime()
	require.True(t, ok)
	require.Equal(t, now, lastSuccessfulSnapshot)
}

type timesInOrder []xtime.UnixNano

func (a timesInOrder) Len() int           { return len(a) }
func (a timesInOrder) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a timesInOrder) Less(i, j int) bool { return a[i].Before(a[j]) }
