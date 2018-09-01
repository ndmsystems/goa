package stdlib

import (
	"errors"
	"fmt"
	"runtime"
	"time"
)

//
// Exit process reason constants
//
const (
	ExitNormal   string = "normal"
	ExitKill     string = "kill"
	ExitKilled   string = "killed"
	traceFuncHSM string = "HandleSysMsg"
)

//
// GenProcSys is a default implementation of GenProc interface
//
type GenProcSys struct {
	pid          *Pid
	trapExit     bool
	links        []*Pid
	monitorsByMe map[Ref]*Pid
	monitors     map[Ref]*Pid
	genProc      GenProcFunc
	tracer       Tracer
}

//
// NewGenProcSys creates GenProcSys object and returns it as GenProc interface
//
func NewGenProcSys(f GenProcFunc) GenProc {
	gps := &GenProcSys{}
	gps.genProc = f

	return gps
}

//
// SetTrapExit sets trap_exit flag for the process
//
func (gps *GenProcSys) SetTrapExit(flag bool) {
	gps.trapExit = flag
}

//
// TrapExit returns trap_exit flag of the process
//
func (gps *GenProcSys) TrapExit() bool {
	return gps.trapExit
}

//
// Self returns pid of the process
//
func (gps *GenProcSys) Self() *Pid {
	return gps.pid
}

func (gps *GenProcSys) setPid(pid *Pid) {
	gps.pid = pid
}

//
// SetTracer sets tracer for the process
//
func (gps *GenProcSys) SetTracer(t Tracer) {
	gps.tracer = t
}

//
// Tracer return current tracer of the process
//
func (gps *GenProcSys) Tracer() Tracer {
	return gps.tracer
}

//
// HandleSysMsg handles system messages for the process
//
func (gps *GenProcSys) HandleSysMsg(msg *SysReq) (err error) {

	TraceCall(gps.Tracer(), gps.Self(), traceFuncHSM, msg)
	ts := time.Now()
	defer TraceCallResult(gps.Tracer(), gps.Self(), &ts, traceFuncHSM, msg, err)

	switch r := msg.Data.(type) {

	case *LinkPidReq:
		gps.link(r.Pid)

	case *UnlinkPidReq:
		_ = gps.unlink(r.Pid)

	case *ProcessLinksReq:
		r.Links = gps.processLinks()
		msg.ReplyChan <- true

	case *ExitPidReq:

		err = gps.doExitPid(r)

	case *StopPidReq:

		err = fmt.Errorf(r.Reason)
	//
	// Monitors
	//
	case *MonitorPidReq:

		gps.monitorMe(r.PidFrom, r.MonitorRef)

	case *DemonitorPidReq:

		gps.demonitorMe(r.MonitorRef)

	case *MonitorDownReq:

		gps.demonitorByMe(r.MonitorRef)
		_ = gps.pid.SendInfo(r)
	}

	return
}

func (gps *GenProcSys) doExitPid(r *ExitPidReq) error {

	var wasLinked bool
	if r.Exit {
		wasLinked = gps.unlink(r.From)
	}

	//
	// drop exit from dying process that were not linked
	//
	if r.Exit && !wasLinked {
		return nil
	}

	fromMyPid := r.From == nil
	exitReason := r.Reason

	//
	// ignore normal exit from external processes
	//
	if !fromMyPid && exitReason == ExitNormal && !gps.TrapExit() {
		return nil
	}

	//
	// exit message from other linked pid
	//
	if !fromMyPid && exitReason != ExitKill && gps.TrapExit() {
		//
		// redirect message to usr channel
		//
		_ = gps.pid.SendInfo(r)

		return nil
	}

	if exitReason == ExitKill {
		exitReason = ExitKilled
	}

	return errors.New(exitReason)
}

//
// InitPrepare is empty for GenProc
//
func (gps *GenProcSys) InitPrepare() {
}

//
// InitAck used for reply to calles the result of the process initialization
//
func (gps *GenProcSys) InitAck() error {

	return nil
}

//
// GenProcLoop implements process loop
//
func (gps *GenProcSys) GenProcLoop(args ...Term) error {
	return gps.genProc(gps, args...)
}

//
// Link sets link to pid
//
func (gps *GenProcSys) Link(pid *Pid) {
	if gps.link(pid) {
		err := gps.pid.link(pid)
		if err != nil {
			_ = pid.ExitReason(gps.pid, NoProc)
		}
	}
}

//
// Unlink removes the link from pid
//
func (gps *GenProcSys) Unlink(pid *Pid) {
	if gps.unlink(pid) {
		_ = pid.unlink(gps.pid)
	}
}

func (gps *GenProcSys) processLinks() []*Pid {
	if len(gps.links) == 0 {
		return nil
	}

	links := make([]*Pid, len(gps.links))
	for i, pid := range gps.links {
		links[i] = pid
	}

	return links
}

//
// MonitorProcessPid sets monitor for process identified by pid
//
func (gps *GenProcSys) MonitorProcessPid(pid *Pid) Ref {

	ref := gps.pid.env.MakeRef()
	gps.monitorProcessPid(ref, pid)

	return ref
}

func (gps *GenProcSys) monitorProcessPid(ref Ref, pid *Pid) {
	err := pid.SendSys(&MonitorPidReq{ref, gps.pid})
	if err == nil {
		gps.monitorByMe(pid, ref)
	} else {
		//
		// nothing to handle in sys level -> send to usr level
		//
		_ = gps.pid.SendInfo(&MonitorDownReq{ref, pid, err.Error()})
	}
}

//
// DemonitorProcessPid removes the monitor for process identified by ref
//
func (gps *GenProcSys) DemonitorProcessPid(ref Ref) {
	if pid := gps.demonitorByMe(ref); pid != nil {
		_ = pid.SendSys(&DemonitorPidReq{ref})
	}
}

//
// Run starts process
//
func (gps *GenProcSys) Run(gp GenProc, opts *SpawnOpts, args ...Term) {

	var err error
	exitReason := ExitNormal

	defer func() {
		if r := recover(); r != nil {

			exitReason = fmt.Sprintf("%#v", r)
			trace := make([]byte, 512)
			_ = runtime.Stack(trace, true)

			fmt.Printf("%s %s/gp: crashed with reaason %s: %s\n",
				time.Now().Truncate(time.Microsecond), gp.Self(), exitReason,
				trace)

		} else if err != nil {
			exitReason = err.Error()
		}

		TraceCall(
			gp.Tracer(), gp.Self(), "gp: run.defer, exitReason:", exitReason)

		gps.onStop(exitReason)
		gps.flushMessages(gps.pid)
	}()

	//
	// link processes
	//
	// opts.linkPid is a parent proccess, who initiates link
	//
	if opts.linkPid != nil {
		gps.Link(opts.linkPid)
	}

	err = gp.GenProcLoop(args...)

	return
}

//
// Links
//
func (gps *GenProcSys) link(pid *Pid) bool {
	if pid == nil || gps.pid.Equal(pid) {
		return false
	}

	for _, linkedPid := range gps.links {
		if pid.Equal(linkedPid) {
			return false
		}
	}

	gps.links = append(gps.links, pid)

	return true
}

func (gps *GenProcSys) unlink(pid *Pid) bool {
	if pid == nil || gps.links == nil {
		return false
	}

	for i, linkedPid := range gps.links {
		if pid.Equal(linkedPid) {
			gps.links[i] = gps.links[len(gps.links)-1]
			gps.links = gps.links[:len(gps.links)-1]
			return true
		}
	}

	return false
}

//
// Monitors
//
func (gps *GenProcSys) monitorMe(pid *Pid, ref Ref) {
	if gps.monitors == nil {
		gps.monitors = make(map[Ref]*Pid)
	}
	gps.monitors[ref] = pid
}

func (gps *GenProcSys) demonitorMe(ref Ref) {
	if gps.monitors == nil {
		return
	}
	delete(gps.monitors, ref)
}

func (gps *GenProcSys) monitorByMe(pid *Pid, ref Ref) {
	if gps.monitorsByMe == nil {
		gps.monitorsByMe = make(map[Ref]*Pid)
	}
	gps.monitorsByMe[ref] = pid
}

func (gps *GenProcSys) demonitorByMe(ref Ref) *Pid {
	if gps.monitorsByMe == nil {
		return nil
	}

	if pid, ok := gps.monitorsByMe[ref]; ok {
		delete(gps.monitorsByMe, ref)
		return pid
	}

	return nil
}

//
func (gps *GenProcSys) onStop(reason string) {

	//
	// send exit to linked Pids
	//
	for _, linkedPid := range gps.links {
		_ = gps.pid.exitReason(linkedPid, reason, true)
	}
	gps.links = nil

	//
	// send MonitorDownReq to processes who monitors me
	//
	for ref, pid := range gps.monitors {
		gps.pid.monitorDown(pid, ref, reason)
	}
	gps.monitors = nil
	gps.monitorsByMe = nil
}

//
// Channels
//
func (gps *GenProcSys) flushMessages(pid *Pid) {

	defer func() {
		if r := recover(); r != nil {
			now := time.Now().Truncate(time.Microsecond)

			// fmt.Printf("%s %s/gp: flushMessages recovered: %#v\n",
			// 	now, pid, r)

			trace := make([]byte, 512)
			count := runtime.Stack(trace, true)
			fmt.Printf("%s %s/gp: flushMessages stack of %d bytes: %s\n",
				now, pid, count, trace)
		}
	}()

	stop := false

	for stop == false {

		select {

		case m := <-pid.sysChan:

			if m == nil {
				continue
			}

			switch r := m.Data.(type) {

			case *LinkPidReq:
				_ = gps.pid.exitReason(r.Pid, NoProc, true)

			case *MonitorPidReq:
				gps.pid.monitorDown(r.PidFrom, r.MonitorRef, NoProc)

			}

		// case m := <-pid.usrChan:

		// 	if m == nil {
		// 		continue
		// 	}

		// 	switch m := m.(type) {

		// 	case *SyncReq:

		// 		gps.traceCall("gp: flush usr channel", m)
		// 	}

		default:
			stop = true
		}
	}

	close(pid.exitChan)
}
