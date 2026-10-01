package modbus

import (
	"fmt"
	"io"
	"log"
	"os"
	"time"
)

const (
	maxRTUFrameLength int = 256
	// longest time the line is kept waiting for a reply after its request
	// timed out (the settle window is the timeout, up to this)
	maxRTUSettle time.Duration = 1 * time.Second
	// a unit which has missed more requests in a row than this is absent:
	// requests to it are no longer held back for the settle window
	rtuAbsentAfter int = 2
)

// What the master knows about a unit whose requests have timed out.
type rtuUnitState struct {
	// replies to timed out requests which may still arrive
	owed int
	// when the last of them is given up
	owedUntil time.Time
	// requests in a row which got no frame back from the unit
	misses int
}

type rtuTransport struct {
	logger       *logger
	link         rtuLink
	timeout      time.Duration
	lastActivity time.Time
	t35          time.Duration
	t1           time.Duration
	settle       time.Duration
	units        map[uint8]*rtuUnitState
}

type rtuLink interface {
	Close() error
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	SetDeadline(time.Time) error
}

// rxFlusher is implemented by links which can drop the contents of their rx
// buffer without waiting for more data. flushRx returns the number of bytes
// dropped.
type rxFlusher interface {
	flushRx() int
}

// Returns a new RTU transport.
func newRTUTransport(link rtuLink, addr string, speed uint, timeout time.Duration, customLogger *log.Logger) (rt *rtuTransport) {
	rt = &rtuTransport{
		logger:  newLogger(fmt.Sprintf("rtu-transport(%s)", addr), customLogger),
		link:    link,
		timeout: timeout,
		t1:      serialCharTime(speed),
		settle:  timeout,
	}

	if rt.settle > maxRTUSettle {
		rt.settle = maxRTUSettle
	}

	if speed >= 19200 {
		// for baud rates equal to or greater than 19200 bauds, a fixed value of
		// 1750 uS is specified for t3.5.
		rt.t35 = 1750 * time.Microsecond
	} else {
		// for lower baud rates, the inter-frame delay should be 3.5 character times
		rt.t35 = (serialCharTime(speed) * 35) / 10
	}

	return
}

// Closes the rtu link.
func (rt *rtuTransport) Close() (err error) {
	err = rt.link.Close()

	return
}

// Runs a request across the rtu link and returns a response.
func (rt *rtuTransport) ExecuteRequest(req *pdu) (res *pdu, err error) {
	var ts time.Time
	var t time.Duration
	var n int
	var stale *pdu

	// RTU has no transaction id: if this unit may still answer a request which
	// timed out, that reply must be off the line before a new request is sent
	rt.settleUnit(req.unitId)

	// if the line was active less than 3.5 char times ago,
	// let t3.5 expire before transmitting
	t = time.Since(rt.lastActivity.Add(rt.t35))
	if t < 0 {
		time.Sleep(t * (-1))
	}

	// RTU has no transaction id: whatever is already in the rx buffer (e.g. a
	// late reply to a request which timed out) would be read as the answer to
	// this request, so drop it right before transmitting
	rt.flushRx()

	// set an i/o deadline on the link (after the flush, which may move it)
	err = rt.link.SetDeadline(time.Now().Add(rt.timeout))
	if err != nil {
		return
	}

	ts = time.Now()

	// build an RTU ADU out of the request object and
	// send the final ADU+CRC on the wire
	n, err = rt.link.Write(rt.assembleRTUFrame(req))
	if err != nil {
		return
	}

	// estimate how long the serial line was busy for.
	// note that on most platforms, Write() will be buffered and return
	// immediately rather than block until the buffer is drained
	rt.lastActivity = ts.Add(time.Duration(n) * rt.t1)

	// observe inter-frame delays
	time.Sleep(rt.lastActivity.Add(rt.t35).Sub(time.Now()))

	// read the response back from the wire
	for {
		res, err = rt.readRTUFrame()
		if err != nil {
			break
		}

		// a late reply to a request which timed out: never the answer, even
		// if it has the right unit id, function and length
		if rt.isOwedReply(res) {
			continue
		}

		if isResponseTo(req, res) {
			break
		}

		// a valid frame which cannot be the answer (a late reply which
		// arrived after the flush): keep listening for the real one
		rt.logger.Warningf("skipping frame from unit %v, function 0x%02x "+
			"(expected unit %v, function 0x%02x)",
			res.unitId, res.functionCode, req.unitId, req.functionCode)
		stale = res
	}

	// no answer: the unit owes a reply which may still show up
	if err == ErrRequestTimedOut || os.IsTimeout(err) {
		rt.owes(req.unitId)
	}

	// nothing better came in: hand the mismatched frame to the client, which
	// rejects it with the same error as it always has
	if stale != nil && (err == ErrRequestTimedOut || os.IsTimeout(err)) {
		res, err = stale, nil
	}

	if err == ErrBadCRC || err == ErrProtocolError || err == ErrShortFrame {
		// wait for and flush any data coming off the link to allow
		// devices to re-sync
		time.Sleep(time.Duration(maxRTUFrameLength) * rt.t1)
		discard(rt.link)
	}

	// mark the time if we heard anything back
	if err != ErrRequestTimedOut {
		rt.lastActivity = time.Now()
	}

	return
}

// Empties the link's rx buffer ahead of a request.
func (rt *rtuTransport) flushRx() {
	var n int

	if f, ok := rt.link.(rxFlusher); ok {
		// serial ports: no waiting, so inter-frame timing is unaffected
		n = f.flushRx()
	} else {
		n = discard(rt.link)
	}

	if n > 0 {
		rt.logger.Warningf("discarded %v stale byte(s) before sending a request", n)
	}

	return
}

// Books a request to unitId which timed out: its reply may still arrive, for
// as long as the settle window lasts.
func (rt *rtuTransport) owes(unitId uint8) {
	var us *rtuUnitState
	var now = time.Now()

	us = rt.units[unitId]
	if us == nil {
		if rt.units == nil {
			rt.units = make(map[uint8]*rtuUnitState)
		}
		us = &rtuUnitState{}
		rt.units[unitId] = us
	}

	// replies which did not come within their window are given up
	if !now.Before(us.owedUntil) {
		us.owed = 0
	}

	us.owed++
	us.owedUntil = now.Add(rt.settle)
	us.misses++

	return
}

// Books a well-formed frame against the unit which sent it. Returns true if
// that unit still owed a reply to a timed out request: the frame is that late
// reply and must be dropped.
func (rt *rtuTransport) isOwedReply(res *pdu) (owed bool) {
	var us *rtuUnitState

	us = rt.units[res.unitId]
	if us == nil {
		return
	}

	// the unit is alive
	us.misses = 0

	if us.owed > 0 && time.Now().Before(us.owedUntil) {
		us.owed--
		owed = true
		rt.logger.Warningf("dropped a late reply from unit %v, function 0x%02x",
			res.unitId, res.functionCode)
	}

	return
}

// Quarantine: holds the line until unitId has delivered the replies it owes or
// the settle window is over, so that the next frame from it can only be the
// answer to the next request. Returns at once if nothing is owed, or if the
// unit is absent (a dead unit must not cost bus time; isOwedReply still drops
// its late reply should it come back).
func (rt *rtuTransport) settleUnit(unitId uint8) {
	var us *rtuUnitState
	var res *pdu
	var err error

	us = rt.units[unitId]
	if us == nil {
		return
	}

	for us.owed > 0 && us.misses <= rtuAbsentAfter &&
		time.Now().Before(us.owedUntil) {
		rt.link.SetDeadline(us.owedUntil)

		// a frame which has started inside the window is read to its end
		res, err = rt.readRTUFrameWithin(time.Duration(maxRTUFrameLength) * rt.t1)
		if err == nil {
			rt.isOwedReply(res)
			rt.lastActivity = time.Now()
			continue
		}

		// silence until the end of the window
		if err == ErrRequestTimedOut || os.IsTimeout(err) {
			break
		}

		// garbage: let it pass and drop it, as after a bad reply
		time.Sleep(time.Duration(maxRTUFrameLength) * rt.t1)
		discard(rt.link)
		rt.lastActivity = time.Now()
	}

	return
}

// Returns true if res may be the response to req: same function code (or its
// exception), from the same unit (exceptions may also come from gateway
// devices, which use unit id #255).
func isResponseTo(req *pdu, res *pdu) bool {
	if res.functionCode == req.functionCode {
		return res.unitId == req.unitId
	}

	return res.functionCode == (req.functionCode|0x80) &&
		(res.unitId == req.unitId || res.unitId == 0xff)
}

// Reads a request from the rtu link.
func (rt *rtuTransport) ReadRequest() (req *pdu, err error) {
	// reading requests from RTU links is currently unsupported
	err = fmt.Errorf("unimplemented")

	return
}

// Writes a response to the rtu link.
func (rt *rtuTransport) WriteResponse(res *pdu) (err error) {
	var n int

	// build an RTU ADU out of the request object and
	// send the final ADU+CRC on the wire
	n, err = rt.link.Write(rt.assembleRTUFrame(res))
	if err != nil {
		return
	}

	rt.lastActivity = time.Now().Add(rt.t1 * time.Duration(n))

	return
}

// Waits for, reads and decodes a frame from the rtu link.
func (rt *rtuTransport) readRTUFrame() (res *pdu, err error) {
	res, err = rt.readRTUFrameWithin(0)

	return
}

// Same as readRTUFrame, except that if bodyGrace is not zero, a frame whose
// header came in before the link's deadline gets that much time to come in
// whole.
func (rt *rtuTransport) readRTUFrameWithin(bodyGrace time.Duration) (res *pdu, err error) {
	var rxbuf []byte
	var byteCount int
	var bytesNeeded int
	var crc crc

	rxbuf = make([]byte, maxRTUFrameLength)

	// read the serial ADU header: unit id (1 byte), function code (1 byte) and
	// PDU length/exception code (1 byte)
	byteCount, err = io.ReadFull(rt.link, rxbuf[0:3])
	if (byteCount > 0 || err == nil) && byteCount != 3 {
		err = ErrShortFrame
		return
	}
	if err != nil && err != io.ErrUnexpectedEOF {
		return
	}

	// figure out how many further bytes to read
	bytesNeeded, err = expectedResponseLenth(uint8(rxbuf[1]), uint8(rxbuf[2]))
	if err != nil {
		return
	}

	// we need to read 2 additional bytes of CRC after the payload
	bytesNeeded += 2

	if bodyGrace > 0 {
		rt.link.SetDeadline(time.Now().Add(bodyGrace))
	}

	// never read more than the max allowed frame length
	if byteCount+bytesNeeded > maxRTUFrameLength {
		err = ErrProtocolError
		return
	}

	byteCount, err = io.ReadFull(rt.link, rxbuf[3:3+bytesNeeded])
	if err != nil && err != io.ErrUnexpectedEOF {
		return
	}
	if byteCount != bytesNeeded {
		rt.logger.Warningf("expected %v bytes, received %v", bytesNeeded, byteCount)
		err = ErrShortFrame
		return
	}

	// compute the CRC on the entire frame, excluding the CRC
	crc.init()
	crc.add(rxbuf[0 : 3+bytesNeeded-2])

	// compare CRC values
	if !crc.isEqual(rxbuf[3+bytesNeeded-2], rxbuf[3+bytesNeeded-1]) {
		err = ErrBadCRC
		return
	}

	res = &pdu{
		unitId:       rxbuf[0],
		functionCode: rxbuf[1],
		// pass the byte count + trailing data as payload, withtout the CRC
		payload: rxbuf[2 : 3+bytesNeeded-2],
	}

	return
}

// Turns a PDU object into bytes.
func (rt *rtuTransport) assembleRTUFrame(p *pdu) (adu []byte) {
	var crc crc

	adu = append(adu, p.unitId)
	adu = append(adu, p.functionCode)
	adu = append(adu, p.payload...)

	// run the ADU through the CRC generator
	crc.init()
	crc.add(adu)

	// append the CRC to the ADU
	adu = append(adu, crc.value()...)

	return
}

// Computes the expected length of a modbus RTU response.
func expectedResponseLenth(responseCode uint8, responseLength uint8) (byteCount int, err error) {
	switch responseCode {
	case fcReadSlaveId:
		byteCount = int(responseLength)
	case fcReadHoldingRegisters,
		fcReadInputRegisters,
		fcReadCoils,
		fcBootloader,
		fcReadDiscreteInputs:
		byteCount = int(responseLength)
	case fcWriteSingleRegister,
		fcWriteMultipleRegisters,
		fcWriteSingleCoil,
		fcWriteMultipleCoils:
		byteCount = 3
	case fcMaskWriteRegister:
		byteCount = 5
	case fcReadHoldingRegisters | 0x80,
		fcReadInputRegisters | 0x80,
		fcReadCoils | 0x80,
		fcReadDiscreteInputs | 0x80,
		fcWriteSingleRegister | 0x80,
		fcWriteMultipleRegisters | 0x80,
		fcWriteSingleCoil | 0x80,
		fcWriteMultipleCoils | 0x80,
		fcMaskWriteRegister | 0x80,
		fcBootloader | 0x80:
		byteCount = 0
	default:
		err = ErrProtocolError
	}

	return
}

// Discards the contents of the link's rx buffer, eating up to 1kB of data, and
// returns the number of bytes discarded.
// Note that on a serial line, this call may block for up to serialConf.Timeout
// i.e. 10ms.
func discard(link rtuLink) (n int) {
	var rxbuf = make([]byte, 1024)

	link.SetDeadline(time.Now().Add(500 * time.Microsecond))
	n, _ = io.ReadFull(link, rxbuf)

	return
}

// Returns how long it takes to send 1 byte on a serial line at the
// specified baud rate.
func serialCharTime(rate_bps uint) (ct time.Duration) {
	// note: an RTU byte on the wire is:
	// - 1 start bit,
	// - 8 data bits,
	// - 1 parity or stop bit
	// - 1 stop bit
	ct = (11) * time.Second / time.Duration(rate_bps)

	return
}
