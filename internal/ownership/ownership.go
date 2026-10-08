// Package ownership establishes and rechecks the process identity behind one
// tray item. The service terminates only an item whose D-Bus owner still maps
// to the same-UID process it recorded at registration time, so a recycled PID
// or a replaced owner can never receive a signal meant for the old one.
package ownership

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/godbus/dbus/v5"
)

// Record is one established process identity. StartTime is the /proc stat
// starttime field, which pairs with the PID to name one exact process life.
type Record struct {
	UID       uint32
	PID       int
	StartTime uint64
}

// busInterface is the org.freedesktop.DBus peer the queries go to.
const busInterface = "org.freedesktop.DBus"

// ErrUnavailable reports that no trustworthy identity could be established.
var ErrUnavailable = fmt.Errorf("ownership: identity unavailable")

// serviceUID is the daemon's own UID; a terminate request may only target a
// process running as the same user.
var serviceUID = uint32(os.Geteuid())

// Inspect resolves the identity behind one unique bus name. Any failure to
// resolve the user, the PID, or the start time is unavailable: the caller
// must not terminate on a partial identity.
func Inspect(conn *dbus.Conn, owner string) (Record, error) {
	if conn == nil || owner == "" {
		return Record{}, ErrUnavailable
	}
	var uid uint32
	if err := conn.BusObject().Call(busInterface+".GetConnectionUnixUser", 0, owner).Store(&uid); err != nil {
		return Record{}, fmt.Errorf("%w: unix user: %v", ErrUnavailable, err)
	}
	var pid uint32
	if err := conn.BusObject().Call(busInterface+".GetConnectionUnixProcessID", 0, owner).Store(&pid); err != nil {
		return Record{}, fmt.Errorf("%w: unix process id: %v", ErrUnavailable, err)
	}
	if pid == 0 {
		return Record{}, ErrUnavailable
	}
	start, err := readStartTime(int(pid))
	if err != nil {
		return Record{}, err
	}
	return Record{UID: uid, PID: int(pid), StartTime: start}, nil
}

// Verify rechecks a fresh inspection against the recorded identity. A UID
// change, a PID change, or a reused PID with a new start time all reject.
func Verify(recorded, current Record) error {
	if recorded.PID == 0 || current.PID == 0 {
		return ErrUnavailable
	}
	if recorded.UID != current.UID {
		return fmt.Errorf("ownership: uid %d changed to %d", recorded.UID, current.UID)
	}
	if recorded.PID != current.PID {
		return fmt.Errorf("ownership: pid %d changed to %d", recorded.PID, current.PID)
	}
	if recorded.StartTime != current.StartTime {
		return fmt.Errorf("ownership: pid %d recycled", recorded.PID)
	}
	return nil
}

// SameUser reports whether the record belongs to the service's own user.
func SameUser(record Record) bool {
	return record.PID != 0 && record.UID == serviceUID
}

// readStartTime reads field 22 of /proc/<pid>/stat. The comm field may contain
// spaces, so the scan starts after the closing parenthesis.
func readStartTime(pid int) (uint64, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, fmt.Errorf("%w: stat: %v", ErrUnavailable, err)
	}
	text := string(raw)
	end := strings.LastIndex(text, ") ")
	if end < 0 {
		return 0, ErrUnavailable
	}
	fields := strings.Fields(text[end+2:])
	// Fields after ") " start at state (field 3); starttime is field 22, so
	// index 19 within this slice.
	if len(fields) < 20 {
		return 0, ErrUnavailable
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: starttime: %v", ErrUnavailable, err)
	}
	return start, nil
}

// Alive reports whether the recorded process still exists with the same
// start time. A vanished or recycled process is not alive.
func Alive(record Record) bool {
	if record.PID == 0 {
		return false
	}
	start, err := readStartTime(record.PID)
	return err == nil && start == record.StartTime
}

// Signal sends one graceful termination request to the recorded process. The
// kernel rejects a signal to a recycled or vanished PID; the caller treats
// that as the process having left.
func Signal(record Record) error {
	process, err := os.FindProcess(record.PID)
	if err != nil {
		return fmt.Errorf("ownership: find process: %w", err)
	}
	return process.Signal(syscall.SIGTERM)
}
