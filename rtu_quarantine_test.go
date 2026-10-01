package modbus

import (
	"sync"
	"testing"
	"time"
)

// writeLog records when the master put each request on the (fake) wire.
type writeLog struct {
	lock  sync.Mutex
	times []time.Time
}

func (wl *writeLog) add() {
	wl.lock.Lock()
	wl.times = append(wl.times, time.Now())
	wl.lock.Unlock()
}

// Returns the time between write #n and write #n+1 (1-based).
func (wl *writeLog) gap(n int) (d time.Duration) {
	wl.lock.Lock()
	d = wl.times[n].Sub(wl.times[n-1])
	wl.lock.Unlock()

	return
}

// The remaining half of SET-485-02: the slave answers every request after the
// master's timeout, and the next request of the same shape has already been
// written when the late reply comes in. RTU has no transaction id, so the late
// reply used to be returned as the answer to that next request.
func TestRTULateReplyAfterNextRequestIsNotItsAnswer(t *testing.T) {
	var fl = &fakeRTULink{}
	var mc = newTestRTUClient(fl, 50*time.Millisecond)
	var regs []uint16
	var err error

	fl.onWrite = func(n int, frame []byte) {
		// 30ms past the timeout, every time
		time.AfterFunc(80*time.Millisecond, func() {
			fl.deliver(fc03Reply(1, 0x1111*uint16(n), 0x1111*uint16(n)))
		})
	}

	_, err = mc.ReadRegisters(0, 2, HOLDING_REGISTER)
	if err != ErrRequestTimedOut {
		t.Fatalf("request #1 should have timed out, got: %v", err)
	}

	// same unit, same function, same quantity, other registers, sent at once
	regs, err = mc.ReadRegisters(10, 2, HOLDING_REGISTER)
	if err == nil && regs[0] == 0x1111 {
		t.Fatalf("request #2 returned the late reply to request #1: 0x%04x 0x%04x",
			regs[0], regs[1])
	}
	// the slave is too slow for this timeout: the honest result is an error
	if err != ErrRequestTimedOut {
		t.Errorf("request #2 should have timed out, got: %v (%v)", err, regs)
	}
}

// Reply #1 is late, then the slave is quick again: request #2 must get its own
// answer, not the late one in front of it.
func TestRTULateReplyThenPromptReply(t *testing.T) {
	var fl = &fakeRTULink{}
	var mc = newTestRTUClient(fl, 50*time.Millisecond)
	var late = make(chan struct{})
	var regs []uint16
	var err error

	fl.onWrite = func(n int, frame []byte) {
		switch n {
		case 1:
			time.AfterFunc(80*time.Millisecond, func() {
				fl.deliver(fc03Reply(1, 0xaaaa, 0xaaaa))
				close(late)
			})
		case 2:
			// a slave answers its requests in order
			go func() {
				<-late
				fl.deliver(fc03Reply(1, 0xbbbb, 0xbbbb))
			}()
		}
	}

	_, err = mc.ReadRegisters(0, 2, HOLDING_REGISTER)
	if err != ErrRequestTimedOut {
		t.Fatalf("request #1 should have timed out, got: %v", err)
	}

	regs, err = mc.ReadRegisters(10, 2, HOLDING_REGISTER)
	if err != nil {
		t.Fatalf("request #2 should have succeeded, got: %v", err)
	}
	if regs[0] != 0xbbbb || regs[1] != 0xbbbb {
		t.Errorf("request #2 returned the late reply to request #1: 0x%04x 0x%04x",
			regs[0], regs[1])
	}
	if fl.pending() != 0 {
		t.Errorf("%v bytes left in the rx buffer after request #2", fl.pending())
	}
}

// The late frame starts just before the settle window ends and finishes after
// it: it must be read to its end, not cut in two (its tail would land in front
// of the next reply).
func TestRTULateReplyCutByTheSettleWindow(t *testing.T) {
	var fl = &fakeRTULink{}
	// 9600 bauds: a maximum frame takes 293ms, far more than the 40ms needed
	var mc = &ModbusClient{
		logger:     newLogger("test-client", nil),
		endianness: BIG_ENDIAN,
		wordOrder:  HIGH_WORD_FIRST,
		unitId:     1,
		transport:  newRTUTransport(fl, "fake", 9600, 100*time.Millisecond, nil),
	}
	var late = make(chan struct{})
	var regs []uint16
	var err error

	fl.onWrite = func(n int, frame []byte) {
		var reply []byte

		switch n {
		case 1:
			// timeout at 100ms, settle window until 200ms
			reply = fc03Reply(1, 0xaaaa, 0xaaaa)
			time.AfterFunc(180*time.Millisecond, func() {
				fl.deliver(reply[:3])
			})
			time.AfterFunc(220*time.Millisecond, func() {
				fl.deliver(reply[3:])
				close(late)
			})
		case 2:
			go func() {
				<-late
				fl.deliver(fc03Reply(1, 0xbbbb, 0xbbbb))
			}()
		}
	}

	_, err = mc.ReadRegisters(0, 2, HOLDING_REGISTER)
	if err != ErrRequestTimedOut {
		t.Fatalf("request #1 should have timed out, got: %v", err)
	}

	regs, err = mc.ReadRegisters(10, 2, HOLDING_REGISTER)
	if err != nil {
		t.Fatalf("request #2 should have succeeded, got: %v", err)
	}
	if regs[0] != 0xbbbb || regs[1] != 0xbbbb {
		t.Errorf("request #2 returned 0x%04x 0x%04x, expected 0xbbbb 0xbbbb",
			regs[0], regs[1])
	}
}

// A unit which has been silent for a while is not waited for (see
// TestRTUAbsentUnitIsNotWaitedFor), so its late reply can still arrive after
// the next request was written. It must be dropped all the same.
func TestRTUAbsentUnitLateReply(t *testing.T) {
	var fl = &fakeRTULink{}
	var mc = newTestRTUClient(fl, 50*time.Millisecond)
	var regs []uint16
	var err error

	fl.onWrite = func(n int, frame []byte) {
		// silent for four requests, then answering 30ms past the timeout
		if n >= 5 {
			time.AfterFunc(80*time.Millisecond, func() {
				fl.deliver(fc03Reply(1, 0x1111*uint16(n), 0x1111*uint16(n)))
			})
		}
	}

	for n := 1; n <= 5; n++ {
		_, err = mc.ReadRegisters(0, 2, HOLDING_REGISTER)
		if err != ErrRequestTimedOut {
			t.Fatalf("request #%v should have timed out, got: %v", n, err)
		}
	}

	regs, err = mc.ReadRegisters(10, 2, HOLDING_REGISTER)
	if err == nil && regs[0] == 0x5555 {
		t.Fatalf("request #6 returned the late reply to request #5: 0x%04x 0x%04x",
			regs[0], regs[1])
	}
	if err != ErrRequestTimedOut {
		t.Errorf("request #6 should have timed out, got: %v (%v)", err, regs)
	}
}

// What a dead unit costs: the first two requests after a timeout are held back
// for the settle window (a late reply may still come), the following ones are
// not, so an outage costs two settle windows once and nothing after that.
func TestRTUAbsentUnitIsNotWaitedFor(t *testing.T) {
	const timeout = 50 * time.Millisecond

	var fl = &fakeRTULink{}
	var mc = newTestRTUClient(fl, timeout)
	var wl = &writeLog{}
	var err error
	var d time.Duration

	fl.onWrite = func(n int, frame []byte) {
		wl.add()
	}

	for n := 1; n <= 6; n++ {
		_, err = mc.ReadRegisters(0, 2, HOLDING_REGISTER)
		if err != ErrRequestTimedOut {
			t.Fatalf("request #%v should have timed out, got: %v", n, err)
		}
	}

	// settle window == timeout here: held requests leave 2 timeouts apart
	for n := 1; n <= 2; n++ {
		d = wl.gap(n)
		if d < 2*timeout-5*time.Millisecond {
			t.Errorf("request #%v was written %v after request #%v, expected "+
				"it to be held for the settle window (%v)", n+1, d, n, 2*timeout)
		}
	}
	for n := 3; n <= 5; n++ {
		d = wl.gap(n)
		if d > timeout+timeout/2 {
			t.Errorf("request #%v was written %v after request #%v, expected "+
				"no more than the timeout (%v): the unit is absent", n+1, d, n,
				timeout)
		}
	}
}

// The quarantine is per unit: a request to another unit is sent at once, and
// the unit which timed out is still protected when it is asked again.
func TestRTUQuarantineDoesNotHoldOtherUnits(t *testing.T) {
	const timeout = 50 * time.Millisecond

	var fl = &fakeRTULink{}
	var mc = newTestRTUClient(fl, timeout)
	var late = make(chan struct{})
	var regs []uint16
	var err error
	var ts time.Time
	var d time.Duration

	fl.onWrite = func(n int, frame []byte) {
		switch n {
		case 1:
			// unit 1, 30ms past the timeout
			time.AfterFunc(80*time.Millisecond, func() {
				fl.deliver(fc03Reply(1, 0xaaaa, 0xaaaa))
				close(late)
			})
		case 2:
			// unit 2 answers at once
			d = time.Since(ts)
			fl.deliver(fc03Reply(2, 0x2222, 0x2222))
		case 3:
			go func() {
				<-late
				fl.deliver(fc03Reply(1, 0xbbbb, 0xbbbb))
			}()
		}
	}

	_, err = mc.ReadRegisters(0, 2, HOLDING_REGISTER)
	if err != ErrRequestTimedOut {
		t.Fatalf("request #1 should have timed out, got: %v", err)
	}

	ts = time.Now()
	mc.SetUnitId(2)
	regs, err = mc.ReadRegisters(0, 2, HOLDING_REGISTER)
	if err != nil || regs[0] != 0x2222 {
		t.Fatalf("request #2 (unit 2) should have succeeded, got: %v (%v)", err, regs)
	}
	if d > timeout/2 {
		t.Errorf("request #2 (unit 2) was held for %v by unit 1's timeout", d)
	}

	mc.SetUnitId(1)
	regs, err = mc.ReadRegisters(10, 2, HOLDING_REGISTER)
	if err != nil {
		t.Fatalf("request #3 (unit 1) should have succeeded, got: %v", err)
	}
	if regs[0] != 0xbbbb || regs[1] != 0xbbbb {
		t.Errorf("request #3 returned the late reply to request #1: 0x%04x 0x%04x",
			regs[0], regs[1])
	}
}
