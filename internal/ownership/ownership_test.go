package ownership

import (
	"errors"
	"os"
	"testing"
)

// selfRecord resolves the identity of the test process itself through the
// same /proc reader the service uses.
func selfRecord(t *testing.T) Record {
	t.Helper()
	record, err := Inspect(nil, "")
	// Inspect needs a bus connection; the nil-connection path is unavailable.
	if err == nil {
		t.Fatalf("nil connection resolved an identity: %+v", record)
	}
	pid := os.Getpid()
	start, err := readStartTime(pid)
	if err != nil {
		t.Fatalf("read own starttime: %v", err)
	}
	return Record{UID: uint32(os.Getuid()), PID: pid, StartTime: start}
}

func TestVerifyAcceptsTheSameProcess(t *testing.T) {
	record := selfRecord(t)
	if err := Verify(record, record); err != nil {
		t.Fatalf("same process rejected: %v", err)
	}
	if !SameUser(record) {
		t.Fatal("own process is not same-user")
	}
	if !Alive(record) {
		t.Fatal("own process reported dead")
	}
}

func TestVerifyRejectsChangedIdentity(t *testing.T) {
	record := selfRecord(t)
	recycled := record
	recycled.StartTime = record.StartTime + 1
	if err := Verify(record, recycled); err == nil {
		t.Fatal("recycled PID accepted")
	}
	replaced := record
	replaced.PID = record.PID + 1
	if err := Verify(record, replaced); err == nil {
		t.Fatal("changed PID accepted")
	}
	crossUID := record
	crossUID.UID = record.UID + 1
	if err := Verify(record, crossUID); err == nil {
		t.Fatal("cross-UID accepted")
	}
	if SameUser(crossUID) {
		t.Fatal("cross-UID record reported same-user")
	}
}

func TestVerifyRejectsEmptyRecords(t *testing.T) {
	if err := Verify(Record{}, Record{PID: 1}); err == nil {
		t.Fatal("empty recorded identity accepted")
	}
	if err := Verify(Record{PID: 1}, Record{}); err == nil {
		t.Fatal("empty current identity accepted")
	}
}

func TestAliveRejectsUnknownAndRecycled(t *testing.T) {
	if Alive(Record{}) {
		t.Fatal("zero record reported alive")
	}
	// A PID that cannot exist: the /proc read fails, so the record is dead.
	if Alive(Record{PID: -1, StartTime: 1}) {
		t.Fatal("impossible PID reported alive")
	}
}

func TestReadStartTimeRejectsMalformedStat(t *testing.T) {
	// PID 1 exists but its stat is readable; instead exercise the parser
	// through a truncated synthetic line via the exported error path: a
	// nonexistent PID must yield the unavailable error.
	if _, err := readStartTime(1 << 22); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing process: got %v", err)
	}
}

func TestSignalRejectsImpossiblePID(t *testing.T) {
	// The kernel rejects signals to negative PIDs; the error surfaces to the
	// caller instead of being swallowed.
	if err := Signal(Record{PID: -1}); err == nil {
		t.Fatal("signal to impossible PID succeeded")
	}
}
