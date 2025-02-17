// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 && linux

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"unsafe"
)

// InjectDebugCall injects a debugger call to fn into g. regArgs must
// contain any arguments to fn that are passed in registers, according
// to the internal Go ABI. It may be nil if no arguments are passed in
// registers to fn. args must be a pointer to a valid call frame (including
// arguments and return space) for fn, or nil. tkill must be a function that
// will send SIGTRAP to thread ID tid. gp must be locked to its OS thread and
// running.
//
// On success, InjectDebugCall returns the panic value of fn or nil.
// If fn did not panic, its results will be available in args.
func InjectDebugCall(gp *g, fn interface{}, regArgs *abi.RegArgs, stackArgs interface{}, tkill func(tid int) error, returnOnUnsafePoint bool) (interface{}, error) {
	if gp.lockedm == 0 {
		return nil, plainError("goroutine not locked to thread")
	}

	tid := int(gp.lockedm.ptr().procid)
	if tid == 0 {
		return nil, plainError("missing tid")
	}

	f := efaceOf(&fn)
	if f._type == nil || f._type.kind&kindMask != kindFunc {
		return nil, plainError("fn must be a function")
	}
	fv := (*funcval)(f.data)

	a := efaceOf(&stackArgs)
	if a._type != nil && a._type.kind&kindMask != kindPtr {
		return nil, plainError("args must be a pointer or nil")
	}
	argp := a.data
	var argSize uintptr
	if argp != nil {
		argSize = (*ptrtype)(unsafe.Pointer(a._type)).elem.size
	}

	h := new(debugCallHandler)
	h.gp = gp
	// gp may not be running right now, but we can still get the M
	// it will run on since it's locked.
	h.mp = gp.lockedm.ptr()
	h.fv, h.regArgs, h.argp, h.argSize = fv, regArgs, argp, argSize
	h.handleF = h.handle // Avoid allocating closure during signal

	defer func() { testSigtrap = nil }()
	for i := 0; ; i++ {
		testSigtrap = h.inject
		noteclear(&h.done)
		h.err = ""

		if err := tkill(tid); err != nil {
			return nil, err
		}
		// Wait for completion.
		notetsleepg(&h.done, -1)
		if h.err != "" {
			switch h.err {
			case "call not at safe point":
				if returnOnUnsafePoint {
					// This is for TestDebugCallUnsafePoint.
					return nil, h.err
				}
				fallthrough
			case "retry _Grunnable", "executing on Go runtime stack", "call from within the Go runtime":
				// These are transient states. Try to get out of them.
				if i < 100 {
					usleep(100)
					Gosched()
					continue
				}
			}
			return nil, h.err
		}
		return h.panic, nil
	}
}

type debugCallHandler struct {
	gp      *g
	mp      *m
	fv      *funcval
	regArgs *abi.RegArgs
	argp    unsafe.Pointer
	argSize uintptr
	panic   interface{}

	handleF func(info *siginfo, ctxt *sigctxt, gp2 *g) bool

	err       plainError
	done      note
	savedRegs sigcontext
	savedFP   fpstate1
}

func (h *debugCallHandler) inject(info *siginfo, ctxt *sigctxt, gp2 *g) bool {
	switch h.gp.atomicstatus {
	case _Grunning:
		if getg().m != h.mp {
			println("trap on wrong M", getg().m, h.mp)
			return false
		}
		// Push current PC on the stack.
		rsp := ctxt.rsp() - goarch.PtrSize
		*(*uint64)(unsafe.Pointer(uintptr(rsp))) = ctxt.rip()
		ctxt.set_rsp(rsp)
		// Write the argument frame size.
		*(*uintptr)(unsafe.Pointer(uintptr(rsp - 16))) = h.argSize
		// Save current registers.
		h.savedRegs = *ctxt.regs()
		h.savedFP = *h.savedRegs.fpstate
		h.savedRegs.fpstate = nil
		// Set PC to debugCallV2.
		ctxt.set_rip(uint64(abi.FuncPCABIInternal(debugCallV2)))
		// Call injected. Switch to the debugCall protocol.
		testSigtrap = h.handleF
	case _Grunnable:
		// Ask InjectDebugCall to pause for a bit and then try
		// again to interrupt this goroutine.
		h.err = plainError("retry _Grunnable")
		notewakeup(&h.done)
	default:
		h.err = plainError("goroutine in unexpected state at call inject")
		notewakeup(&h.done)
	}
	// Resume execution.
	return true
}

func (h *debugCallHandler) handle(info *siginfo, ctxt *sigctxt, gp2 *g) bool {
	// Sanity check.
	if getg().m != h.mp {
		println("trap on wrong M", getg().m, h.mp)
		return false
	}
	f := findfunc(uintptr(ctxt.rip()))
	if !(hasPrefix(funcname(f), "runtime.debugCall") || hasPrefix(funcname(f), "debugCall")) {
		println("trap in unknown function", funcname(f))
		return false
	}
	if *(*byte)(unsafe.Pointer(uintptr(ctxt.rip() - 1))) != 0xcc {
		println("trap at non-INT3 instruction pc =", hex(ctxt.rip()))
		return false
	}

	switch status := ctxt.r12(); status {
	case 0:
		// Frame is ready. Copy the arguments to the frame and to registers.
		sp := ctxt.rsp()
		memmove(unsafe.Pointer(uintptr(sp)), h.argp, h.argSize)
		if h.regArgs != nil {
			storeRegArgs(ctxt.regs(), h.regArgs)
		}
		// Push return PC.
		sp -= goarch.PtrSize
		ctxt.set_rsp(sp)
		*(*uint64)(unsafe.Pointer(uintptr(sp))) = ctxt.rip()
		// Set PC to call and context register.
		ctxt.set_rip(uint64(h.fv.fn))
		ctxt.regs().rdx = uint64(uintptr(unsafe.Pointer(h.fv)))
	case 1:
		// Function returned. Copy frame and result registers back out.
		sp := ctxt.rsp()
		memmove(h.argp, unsafe.Pointer(uintptr(sp)), h.argSize)
		if h.regArgs != nil {
			loadRegArgs(h.regArgs, ctxt.regs())
		}
	case 2:
		// Function panicked. Copy panic out.
		sp := ctxt.rsp()
		memmove(unsafe.Pointer(&h.panic), unsafe.Pointer(uintptr(sp)), 2*goarch.PtrSize)
	case 8:
		// Call isn't safe. Get the reason.
		sp := ctxt.rsp()
		reason := *(*string)(unsafe.Pointer(uintptr(sp)))
		h.err = plainError(reason)
		// Don't wake h.done. We need to transition to status 16 first.
	case 16:
		// Restore all registers except RIP and RSP.
		rip, rsp := ctxt.rip(), ctxt.rsp()
		fp := ctxt.regs().fpstate
		*ctxt.regs() = h.savedRegs
		ctxt.regs().fpstate = fp
		*fp = h.savedFP
		ctxt.set_rip(rip)
		ctxt.set_rsp(rsp)
		// Done
		notewakeup(&h.done)
	default:
		h.err = plainError("unexpected debugCallV2 status")
		notewakeup(&h.done)
	}
	// Resume execution.
	return true
}
