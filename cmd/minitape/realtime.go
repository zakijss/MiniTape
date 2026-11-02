package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"github.com/zakijss/minideck/procs"
)

// While the Onion menu is open, the menu keeps its own audio stream open on
// Onion's audioserver, and the audioserver shares its time between that
// stream and ffplay's. At normal priority (or a raised nice level) ffplay's
// music then plays about 1.5 times too slowly: the audioserver runs short
// and the gaps are heard as cuts. Measured on a Mini Plus: 20 s of music
// took 31-46 s with the menu open, and 20.3 s with ffplay and the
// audioserver under SCHED_FIFO (20.3 s is also what it takes with the menu
// closed). So both run with real-time scheduling while music plays. Both
// block on the sound device most of the time (ffplay uses about 3% of the
// CPU), and the kernel keeps 5% of every second for other processes
// whatever real-time tasks do (sched_rt_runtime_us), so nothing can lock the
// device up. The audioserver is Onion's: it goes back to normal scheduling
// as soon as the music stops, pauses or a game starts.

// audioPriority is the SCHED_FIFO priority (1-99) for the audio path.
const audioPriority = 20

const (
	schedOther = 0
	schedFIFO  = 1
)

// setScheduling sets the scheduling policy of process pid (its main
// thread, as chrt -p does).
func setScheduling(pid, policy, prio int) error {
	param := struct{ prio int32 }{int32(prio)}
	_, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_SETSCHEDULER, uintptr(pid), uintptr(policy), uintptr(unsafe.Pointer(&param)))
	if errno != 0 {
		return errno
	}
	return nil
}

// chrtPath is the device's chrt (BusyBox), used to start ffplay with
// real-time scheduling, so that every thread it starts has it; "" if there
// is none.
var chrtPath = func() string {
	if os.Getenv("MINIDECK_FFPLAY_RT") == "0" {
		return ""
	}
	p, err := exec.LookPath("chrt")
	if err != nil {
		return ""
	}
	return p
}()

// ffplayCommand is the command that runs ffplay with args: under chrt when
// there is one (chrt replaces itself with ffplay, so the process is
// ffplay's: pausing and stopping it work the same).
func ffplayCommand(ffplay string, args ...string) *exec.Cmd {
	if chrtPath == "" {
		return exec.Command(ffplay, args...)
	}
	return exec.Command(chrtPath, append([]string{"-f", strconv.Itoa(audioPriority), ffplay}, args...)...)
}

// audioBoost gives Onion's audioserver real-time scheduling while music
// plays, and gives it back after.
type audioBoost struct {
	pid  func() int // the audioserver's pid, 0 if none
	set  func(pid, policy, prio int) error
	nice func(pid, n int) error // the fallback when set fails
	on   int                    // the pid boosted, 0 if none
}

func newAudioBoost() *audioBoost {
	return &audioBoost{
		pid: func() int { return procs.Pid("audioserver") },
		set: setScheduling,
		nice: func(pid, n int) error {
			return syscall.Setpriority(syscall.PRIO_PROCESS, pid, n)
		},
	}
}

// update boosts the audioserver while playing, and puts it back when not.
// It is cheap to call every second: it only acts on a change (music
// starting or stopping, or a new audioserver process).
func (b *audioBoost) update(playing bool) {
	pid := 0
	if playing {
		pid = b.pid()
	}
	if pid == b.on {
		return
	}
	if b.on != 0 {
		b.set(b.on, schedOther, 0)
		b.nice(b.on, 0)
	}
	b.on = 0
	if pid != 0 {
		if b.set(pid, schedFIFO, audioPriority) != nil {
			b.nice(pid, -10) // not allowed real-time: a raised priority
		}
		b.on = pid
	}
}
