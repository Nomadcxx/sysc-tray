package state

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

func TestOwnerPublishesOneOrderedDeltaPerChange(t *testing.T) {
	owner := Start()
	defer owner.Close()
	sink := attach(t, owner)

	key := itemKey(1)
	owner.Add(key)
	owner.Update(key, sample(key, "Chat"))
	owner.Update(key, sample(key, "Chat (2)"))
	owner.UpdateMenu(key, sampleMenu(3))
	owner.Remove(key)

	kinds := []string{
		protocol.KindItemAdded, protocol.KindItemChanged,
		protocol.KindMenuUpdated, protocol.KindItemRemoved,
	}
	previous := sink.baseline
	for _, want := range kinds {
		envelope := sink.await(t)
		if envelope.Kind != want {
			t.Fatalf("delta kind = %q, want %q", envelope.Kind, want)
		}
		if err := envelope.Validate(); err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if err := protocol.ValidateNextSequence(previous, envelope.Sequence); err != nil {
			t.Fatal(err)
		}
		previous = envelope.Sequence
	}
	sink.expectQuiet(t)
}

func TestOwnerSnapshotPrecedesContiguousDeltas(t *testing.T) {
	owner := Start()
	defer owner.Close()

	first := itemKey(1)
	owner.Add(first)
	owner.Update(first, sample(first, "First"))

	sink := attach(t, owner)
	if len(sink.snapshot.Items) != 1 {
		t.Fatalf("snapshot carried %d items, want 1", len(sink.snapshot.Items))
	}
	if err := sink.snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	second := itemKey(2)
	owner.Add(second)
	owner.Update(second, sample(second, "Second"))
	envelope := sink.await(t)
	if err := protocol.ValidateNextSequence(sink.snapshot.Sequence, envelope.Sequence); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerRejectsResultsFromASupersededGeneration(t *testing.T) {
	owner := Start()
	defer owner.Close()
	sink := attach(t, owner)

	stale := itemKey(1)
	owner.Add(stale)
	owner.Update(stale, sample(stale, "Old"))
	sink.await(t)

	owner.Remove(stale)
	sink.await(t)

	fresh := stale
	fresh.Generation = 2
	owner.Add(fresh)
	owner.Update(fresh, sample(fresh, "New"))
	sink.await(t)

	owner.Update(stale, sample(stale, "Ghost"))
	owner.UpdateMenu(stale, sampleMenu(9))
	sink.expectQuiet(t)
}

func TestOwnerReplacesAGenerationAsRemoveThenAdd(t *testing.T) {
	owner := Start()
	defer owner.Close()
	sink := attach(t, owner)

	first := itemKey(1)
	owner.Add(first)
	owner.Update(first, sample(first, "First"))
	if got := sink.await(t).Kind; got != protocol.KindItemAdded {
		t.Fatalf("first publication = %q", got)
	}

	second := first
	second.Generation = 2
	owner.Add(second)
	owner.Update(second, sample(second, "Second"))

	if got := sink.await(t).Kind; got != protocol.KindItemRemoved {
		t.Fatalf("replacement did not remove the old generation first, got %q", got)
	}
	if got := sink.await(t).Kind; got != protocol.KindItemAdded {
		t.Fatalf("replacement did not add the new generation, got %q", got)
	}
	if items := owner.Snapshot().Items; len(items) != 1 || items[0].Key.Generation != 2 {
		t.Fatalf("snapshot after replacement = %+v", items)
	}
}

func TestOwnerEnforcesTheItemCapWithoutDisturbingExistingItems(t *testing.T) {
	owner := Start()
	defer owner.Close()

	for i := 1; i <= protocol.MaxItems; i++ {
		key := itemKey(uint64(i))
		key.Owner = fmt.Sprintf(":1.%d", i)
		owner.Add(key)
		owner.Update(key, sample(key, "Item"))
	}
	if got := len(owner.Snapshot().Items); got != protocol.MaxItems {
		t.Fatalf("snapshot holds %d items, want %d", got, protocol.MaxItems)
	}

	overflow := itemKey(uint64(protocol.MaxItems + 1))
	overflow.Owner = ":1.overflow"
	owner.Add(overflow)
	owner.Update(overflow, sample(overflow, "Rejected"))

	snapshot := owner.Snapshot()
	if len(snapshot.Items) != protocol.MaxItems {
		t.Fatalf("the cap admitted %d items", len(snapshot.Items))
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, item := range snapshot.Items {
		if item.Key.Owner == overflow.Owner {
			t.Fatal("a rejected registration reached the snapshot")
		}
	}
}

func TestOwnerKeepsSiblingsWhenOneItemIsInvalid(t *testing.T) {
	owner := Start()
	defer owner.Close()
	sink := attach(t, owner)

	healthy := itemKey(1)
	owner.Add(healthy)
	owner.Update(healthy, sample(healthy, "Healthy"))
	sink.await(t)

	broken := itemKey(2)
	broken.Owner = ":1.99"
	owner.Add(broken)
	owner.Update(broken, protocol.Item{Key: broken})
	sink.expectQuiet(t)

	if items := owner.Snapshot().Items; len(items) != 1 || items[0].Key != healthy {
		t.Fatalf("an invalid sibling disturbed the snapshot: %+v", items)
	}
}

func TestOwnerPublishesRecordsThatCannotBeMutatedAfterwards(t *testing.T) {
	owner := Start()
	defer owner.Close()

	key := itemKey(1)
	owner.Add(key)
	owner.Update(key, sample(key, "Chat"))

	snapshot := owner.Snapshot()
	if len(snapshot.Items[0].Icon.Pixmaps[0].ARGB) == 0 {
		t.Fatal("sample item carries no pixmap bytes")
	}
	snapshot.Items[0].Title = "tampered"
	snapshot.Items[0].Icon.Pixmaps[0].ARGB[0] = 0xff

	again := owner.Snapshot()
	if again.Items[0].Title != "Chat" {
		t.Fatalf("published title became %q", again.Items[0].Title)
	}
	if again.Items[0].Icon.Pixmaps[0].ARGB[0] != 0 {
		t.Fatal("published pixmap bytes were mutated through a snapshot")
	}
}

func TestOwnerDropsAnOverflowingPresenterWithoutBlocking(t *testing.T) {
	owner := Start()
	defer owner.Close()
	sink := &recordingSink{fail: errors.New("queue full")}
	if _, err := owner.Attach(sink); err != nil {
		t.Fatal(err)
	}

	key := itemKey(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		owner.Add(key)
		owner.Update(key, sample(key, "Chat"))
		owner.Update(key, sample(key, "Chat (2)"))
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("an overflowing presenter blocked the state owner")
	}

	if got := sink.count(); got != 1 {
		t.Fatalf("owner sent %d messages to a failed sink, want to stop after the first", got)
	}
	if items := owner.Snapshot().Items; len(items) != 1 {
		t.Fatalf("dropping the presenter disturbed state: %+v", items)
	}
}

func attach(t *testing.T, owner *Owner) *recordingSink {
	t.Helper()
	sink := &recordingSink{envelopes: make(chan protocol.Envelope, 64)}
	snapshot, err := owner.Attach(sink)
	if err != nil {
		t.Fatal(err)
	}
	sink.snapshot = snapshot
	sink.baseline = snapshot.Sequence
	return sink
}

type recordingSink struct {
	envelopes chan protocol.Envelope
	snapshot  protocol.Snapshot
	baseline  uint64
	fail      error

	mu   sync.Mutex
	sent int
}

func (s *recordingSink) Send(envelope protocol.Envelope) error {
	s.mu.Lock()
	s.sent++
	s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.envelopes <- envelope
	return nil
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

func (s *recordingSink) await(t *testing.T) protocol.Envelope {
	t.Helper()
	select {
	case envelope := <-s.envelopes:
		return envelope
	case <-time.After(3 * time.Second):
		t.Fatal("no delta published")
		return protocol.Envelope{}
	}
}

func (s *recordingSink) expectQuiet(t *testing.T) {
	t.Helper()
	select {
	case envelope := <-s.envelopes:
		t.Fatalf("unexpected delta %q", envelope.Kind)
	case <-time.After(250 * time.Millisecond):
	}
}

func itemKey(generation uint64) protocol.ItemKey {
	return protocol.ItemKey{Owner: ":1.7", ObjectPath: "/StatusNotifierItem", Generation: generation}
}

func sample(key protocol.ItemKey, title string) protocol.Item {
	return protocol.Item{
		Key: key, ID: "sample", Title: title,
		Category: protocol.CategoryApplicationStatus, Status: protocol.StatusActive,
		Icon: protocol.Icon{Name: "sample", Pixmaps: []protocol.Pixmap{
			{Width: 1, Height: 1, ARGB: make([]byte, 4)},
		}},
	}
}

func sampleMenu(revision uint32) protocol.Menu {
	return protocol.Menu{Revision: revision, Root: protocol.MenuNode{ID: 0, Visible: true,
		Children: []protocol.MenuNode{{ID: 1, Label: "Open", Enabled: true, Visible: true}}}}
}

// A read that found nothing new is not an event. Without this the ownership
// refresh republished every registration, and an application emitting property
// signals on a timer spent a marshal and a frame each time to say nothing.
func TestUpdateWithNoChangePublishesNothing(t *testing.T) {
	owner := Start()
	defer func() { _ = owner.Close() }()
	sink := attach(t, owner)
	key := protocol.ItemKey{Owner: ":1.7", ObjectPath: "/StatusNotifierItem", Generation: 1}
	owner.Add(key)
	item := protocol.Item{
		Key: key, ID: "chat", Title: "Chat",
		Category: protocol.CategoryApplicationStatus, Status: protocol.StatusActive,
		Icon: protocol.Icon{Name: "chat"},
	}
	owner.Update(key, item)
	after := sink.count()
	if after == 0 {
		t.Fatal("the first update published nothing")
	}

	owner.Update(key, item)
	if got := sink.count(); got != after {
		t.Fatalf("an identical update published %d extra envelope(s)", got-after)
	}

	changed := item
	changed.Title = "Chat (2)"
	owner.Update(key, changed)
	if got := sink.count(); got != after+1 {
		t.Fatalf("a real change published %d envelopes, want one", got-after)
	}
}
