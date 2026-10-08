package integration

import (
	"bufio"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/internal/app"
	"github.com/Nomadcxx/sysc-tray/protocol"
)

// TestTerminateHelperProcess is the item owner for the terminate checks: a
// real separate process, so a graceful termination actually ends it and the
// bus name vanishes the way it does for a real tray application.
func TestTerminateHelperProcess(t *testing.T) {
	if os.Getenv("SYSC_TRAY_TERMINATE_HELPER") != "1" {
		return
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		t.Fatalf("helper could not reach the session bus: %v", err)
	}
	fake := &trayApp{conn: conn, props: baseProps("helper", "Helper")}
	if err := conn.Export(itemProperties{app: fake}, itemPath, "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Export(itemMethods{app: fake}, itemPath, itemInterface); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		call := conn.Object(watcherName, watcherPath).Call(
			watcherName+".RegisterStatusNotifierItem", 0, string(itemPath))
		if call.Err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper registration never succeeded: %v", call.Err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stdout.WriteString(conn.Names()[0] + "\n"); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SYSC_TRAY_HELPER_STUBBORN") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	time.Sleep(time.Hour)
}

// terminateHelper spawns the helper process and returns it together with a
// channel closed once it has been reaped and the unique bus name it
// connected under.
func terminateHelper(t *testing.T, stubborn bool) (*exec.Cmd, <-chan struct{}, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestTerminateHelperProcess$", "--")
	cmd.Env = append(os.Environ(), "SYSC_TRAY_TERMINATE_HELPER=1")
	if stubborn {
		cmd.Env = append(cmd.Env, "SYSC_TRAY_HELPER_STUBBORN=1")
	}
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
	})
	name, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("helper never announced its bus name: %v", err)
	}
	return cmd, waited, strings.TrimSpace(name)
}

func TestTerminateClosesARegisteredItem(t *testing.T) {
	c := service(t)
	cmd, waited, _ := terminateHelper(t, false)

	item := c.awaitItem(t, func(i protocol.Item) bool {
		return i.Title == "Helper" && i.CloseSupported
	})

	reply := c.send(t, protocol.Command{Kind: protocol.CommandTerminate, Item: item.Key})
	if !reply.OK || reply.Error != nil {
		t.Fatalf("terminate reply = %+v, want success", reply)
	}
	c.awaitRemoval(t, item.Key)

	// The helper must have died from the signal, not on its own schedule.
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("helper survived the termination request")
	}
	if state := cmd.ProcessState; state == nil || state.Success() || state.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Fatalf("helper exit = %v, want death by SIGTERM", state)
	}
}

func TestTerminateTimesOutOnAStubbornProcess(t *testing.T) {
	wait, poll := app.TerminateWait, app.TerminatePoll
	app.TerminateWait, app.TerminatePoll = 300*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { app.TerminateWait, app.TerminatePoll = wait, poll })

	c := service(t)
	_, _, _ = terminateHelper(t, true)

	item := c.awaitItem(t, func(i protocol.Item) bool {
		return i.Title == "Helper" && i.CloseSupported
	})

	reply := c.send(t, protocol.Command{Kind: protocol.CommandTerminate, Item: item.Key})
	if reply.OK || reply.Error == nil || reply.Error.Code != protocol.ErrorBusy {
		t.Fatalf("terminate of a stubborn process replied %+v, want busy", reply)
	}

	// A failed termination leaves the item visible: a follow-up activate on
	// the same key must still reach the helper rather than answer stale_item.
	probe := c.send(t, protocol.Command{Kind: protocol.CommandActivate, Item: item.Key})
	if !probe.OK || probe.Error != nil {
		t.Fatalf("item vanished after a busy termination: activate replied %+v", probe)
	}
}
