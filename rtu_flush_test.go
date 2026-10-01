package modbus

import (
	"sync"
	"testing"
	"time"
)

// fakeRTULink is an in-memory serial line seen from the master: bytes handed
// to deliver() sit in a receive buffer until Read() takes them, the way they
// sit in the kernel's tty buffer on a real port.
type fakeRTULink struct {
	lock     sync.Mutex
	rxbuf    []byte
	deadline time.Time
	writes   int
	// called (without the lock) each time the master transmits a frame
	onWrite func(n int, frame []byte)
}

func (fl *fakeRTULink) deliver(frame []byte) {
	fl.lock.Lock()
	fl.rxbuf = append(fl.rxbuf, frame...)
	fl.lock.Unlock()
}

func (fl *fakeRTULink) pending() (n int) {
	fl.lock.Lock()
	n = len(fl.rxbuf)
	fl.lock.Unlock()

	return
}

func (fl *fakeRTULink) Close() (err error) { return }

func (fl *fakeRTULink) Read(buf []byte) (n int, err error) {
	for {
		fl.lock.Lock()
		if len(fl.rxbuf) > 0 {
			n = copy(buf, fl.rxbuf)
			fl.rxbuf = fl.rxbuf[n:]
			fl.lock.Unlock()
			return
		}
		if time.Now().After(fl.deadline) {
			fl.lock.Unlock()
			err = ErrRequestTimedOut
			return
		}
		fl.lock.Unlock()
		time.Sleep(50 * time.Microsecond)
	}
}

func (fl *fakeRTULink) Write(buf []byte) (n int, err error) {
	var cb func(int, []byte)
	var count int

	fl.lock.Lock()
	fl.writes++
	count = fl.writes
	cb = fl.onWrite
	fl.lock.Unlock()

	if cb != nil {
		cb(count, append([]byte{}, buf...))
	}
	n = len(buf)

	return
}

func (fl *fakeRTULink) SetDeadline(deadline time.Time) (err error) {
	fl.lock.Lock()
	fl.deadline = deadline
	fl.lock.Unlock()

	return
}

// fakeFlushLink is a fakeRTULink that can empty its receive buffer without
// waiting, like the serial port wrapper.
type fakeFlushLink struct {
	fakeRTULink
	flushes       int
	writesAtFlush []int
	deadlineSets  int
}

func (ffl *fakeFlushLink) flushRx() (n int) {
	ffl.lock.Lock()
	n = len(ffl.rxbuf)
	ffl.rxbuf = nil
	ffl.flushes++
	ffl.writesAtFlush = append(ffl.writesAtFlush, ffl.writes)
	ffl.lock.Unlock()

	return
}

func (ffl *fakeFlushLink) SetDeadline(deadline time.Time) (err error) {
	ffl.lock.Lock()
	ffl.deadlineSets++
	ffl.lock.Unlock()

	return ffl.fakeRTULink.SetDeadline(deadline)
}

// Builds the reply to a "read holding registers" request.
func fc03Reply(unitId uint8, regs ...uint16) (frame []byte) {
	var p = &pdu{
		unitId:       unitId,
		functionCode: fcReadHoldingRegisters,
		payload:      []byte{byte(2 * len(regs))},
	}

	for _, r := range regs {
		p.payload = append(p.payload, uint16ToBytes(BIG_ENDIAN, r)...)
	}
	frame = (&rtuTransport{}).assembleRTUFrame(p)

	return
}

func newTestRTUClient(link rtuLink, timeout time.Duration) (mc *ModbusClient) {
	mc = &ModbusClient{
		logger:     newLogger("test-client", nil),
		endianness: BIG_ENDIAN,
		wordOrder:  HIGH_WORD_FIRST,
		unitId:     1,
		transport:  newRTUTransport(link, "fake", 115200, timeout, nil),
	}

	return
}

// The defect found by SET-485-02: a reply which shows up after its request
// timed out used to be returned as the answer to the next request.
func TestRTULateReplyIsNotTheNextAnswer(t *testing.T) {
	var fl = &fakeRTULink{}
	var mc = newTestRTUClient(fl, 30*time.Millisecond)
	var regs []uint16
	var err error

	fl.onWrite = func(n int, frame []byte) {
		switch n {
		case 1:
			// the device answers request #1 after the master gave up
			time.AfterFunc(60*time.Millisecond, func() {
				fl.deliver(fc03Reply(1, 0xaaaa, 0xaaaa))
			})
		case 2:
			// request #2 is answered in time
			fl.deliver(fc03Reply(1, 0xbbbb, 0xbbbb))
		}
	}

	_, err = mc.ReadRegisters(0, 2, HOLDING_REGISTER)
	if err != ErrRequestTimedOut {
		t.Fatalf("request #1 should have timed out, got: %v", err)
	}

	// let the late reply land in the receive buffer
	time.Sleep(60 * time.Millisecond)
	if fl.pending() == 0 {
		t.Fatalf("the late reply should be sitting in the rx buffer")
	}

	// same unit, same function, same quantity, other registers
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

// Same late reply, but the next request goes to another unit: it used to fail
// with ErrBadUnitId and leave its own (good) reply behind for the request
// after it, and so on.
func TestRTULateReplyDoesNotShiftOtherUnits(t *testing.T) {
	var fl = &fakeRTULink{}
	var mc = newTestRTUClient(fl, 30*time.Millisecond)
	var regs []uint16
	var err error

	fl.onWrite = func(n int, frame []byte) {
		switch n {
		case 1:
			time.AfterFunc(60*time.Millisecond, func() {
				fl.deliver(fc03Reply(1, 0xaaaa))
			})
		default:
			fl.deliver(fc03Reply(frame[0], 0x1100*uint16(n)))
		}
	}

	_, err = mc.ReadRegisters(0, 1, HOLDING_REGISTER)
	if err != ErrRequestTimedOut {
		t.Fatalf("request #1 should have timed out, got: %v", err)
	}
	time.Sleep(60 * time.Millisecond)

	for n, unitId := range []uint8{2, 1, 2} {
		mc.SetUnitId(unitId)
		regs, err = mc.ReadRegisters(0, 1, HOLDING_REGISTER)
		if err != nil {
			t.Fatalf("request #%v (unit %v) should have succeeded, got: %v",
				n+2, unitId, err)
		}
		if regs[0] != 0x1100*uint16(n+2) {
			t.Errorf("request #%v returned 0x%04x, expected 0x%04x",
				n+2, regs[0], 0x1100*uint16(n+2))
		}
	}
}

// A late reply can also arrive after the flush, while the next request is
// going out. If it is recognisably not the answer (other unit or function),
// the real answer behind it must be used.
func TestRTUSkipsFrameOfAnotherRequest(t *testing.T) {
	var fl = &fakeRTULink{}
	var mc = newTestRTUClient(fl, 100*time.Millisecond)
	var regs []uint16
	var err error

	fl.onWrite = func(n int, frame []byte) {
		// unit 1's late reply, then the reply of the unit being asked
		fl.deliver(fc03Reply(1, 0xaaaa))
		fl.deliver(fc03Reply(2, 0xbbbb))
	}

	mc.SetUnitId(2)
	regs, err = mc.ReadRegisters(0, 1, HOLDING_REGISTER)
	if err != nil {
		t.Fatalf("request should have succeeded, got: %v", err)
	}
	if regs[0] != 0xbbbb {
		t.Errorf("expected 0xbbbb, got 0x%04x", regs[0])
	}

	// a read reply in front of a write's reply
	fl.onWrite = func(n int, frame []byte) {
		fl.deliver(fc03Reply(2, 0xaaaa))
		// a write single register reply echoes the request
		fl.deliver(frame)
	}
	err = mc.WriteRegister(5, 0x1234)
	if err != nil {
		t.Errorf("write should have succeeded, got: %v", err)
	}

	// when only a mismatched frame arrives, the caller still gets the error
	// the client has always reported for it
	mc.transport.(*rtuTransport).timeout = 20 * time.Millisecond
	fl.onWrite = func(n int, frame []byte) {
		fl.deliver(fc03Reply(9, 0xaaaa))
	}
	_, err = mc.ReadRegisters(0, 1, HOLDING_REGISTER)
	if err != ErrBadUnitId {
		t.Errorf("expected ErrBadUnitId, got: %v", err)
	}

	fl.onWrite = func(n int, frame []byte) {
		fl.deliver(fc03Reply(2, 0xaaaa))
	}
	err = mc.WriteRegister(5, 0x1234)
	if err != ErrProtocolError {
		t.Errorf("expected ErrProtocolError, got: %v", err)
	}
}

// A link that can flush without waiting (the serial port) is flushed through
// that call, once per request and before the request is written, and the
// transport does not fall back to the timed discard().
func TestRTUFlushesThroughTheLinkBeforeWriting(t *testing.T) {
	var ffl = &fakeFlushLink{}
	var rt = newRTUTransport(ffl, "fake", 115200, 50*time.Millisecond, nil)
	var res *pdu
	var err error

	ffl.onWrite = func(n int, frame []byte) {
		ffl.deliver(fc03Reply(1, 0xbbbb))
	}

	for i := 0; i < 3; i++ {
		// stale bytes from "before": a whole reply with the right shape
		ffl.deliver(fc03Reply(1, 0xaaaa))

		res, err = rt.ExecuteRequest(&pdu{
			unitId:       1,
			functionCode: fcReadHoldingRegisters,
			payload:      []byte{0x00, 0x00, 0x00, 0x01},
		})
		if err != nil {
			t.Fatalf("request #%v should have succeeded, got: %v", i+1, err)
		}
		if res.payload[1] != 0xbb || res.payload[2] != 0xbb {
			t.Errorf("request #%v returned stale data: % x", i+1, res.payload)
		}
	}

	if ffl.flushes != 3 {
		t.Errorf("expected 3 flushes (one per request), saw %v", ffl.flushes)
	}
	for i, w := range ffl.writesAtFlush {
		if w != i {
			t.Errorf("flush #%v happened after %v writes, expected %v", i+1, w, i)
		}
	}
	// discard() would have moved the deadline a second time per request
	if ffl.deadlineSets != 3 {
		t.Errorf("expected 3 deadline updates (one per request), saw %v",
			ffl.deadlineSets)
	}
}
