# RTU master: drop stale receive bytes before every request

Date: 2026-10-01
Branch: `fix/rtu-flush-late-replies` (from `main` = v1.1.2)
Found by: MDI system test SET-485-02 (RS-485 response timeout)

## What happens today

Modbus RTU has no transaction id. The only thing that ties a reply to a
request is the order of events on the wire: the master sends, then whatever
arrives next is "the answer". That only holds if the receive buffer is empty
when the master sends. The RTU transport never makes sure of that.

`rtu_transport.go`, `ExecuteRequest` (lines 59-110 on `main`):

- line 65: sets the i/o deadline
- lines 72-75: waits out t3.5 if the line was active very recently
- line 81: writes the request
- line 98: reads one frame with `readRTUFrame()` and returns it

Nothing is read or discarded between the previous exchange and line 81.
The only flush is at lines 100-105, and only after `ErrBadCRC`,
`ErrProtocolError` or `ErrShortFrame`. In particular:

- After `ErrRequestTimedOut` nothing is flushed (line 100 does not list it).
  A reply that arrives after the deadline stays in the OS receive buffer.
- `readRTUFrame()` (lines 137-199) accepts any frame with a good CRC. It does
  not look at the request.
- The request/response match is done one layer up and is weak:
  `client.go` `executeRequest` (lines 1244-1267) checks the unit id, and each
  operation checks function code and length, e.g. `readRegisters`
  (lines 1124-1145). A reply to the *previous* request to the same unit with
  the same function and quantity passes all of them.
- When one of those checks does fail (`ErrBadUnitId`, `ErrProtocolError`) the
  client just returns. The transport is not told and discards nothing.

Result, as a timeline (timeout 1000 ms):

| t (ms) | wire | master |
|-------:|------|--------|
| 0 | request A (unit 1, FC3, reg 0, qty 2) | waits |
| 1000 | - | A fails: `request timed out` |
| 1200 | reply A arrives (late) | sits in the rx buffer |
| 1400 | request B (unit 1, FC3, reg 10, qty 2) | reads the buffered reply A: CRC ok, unit ok, function ok, length ok |
| 1400 | - | **B returns A's data, no error** |
| 1450 | reply B arrives | sits in the rx buffer |
| next | request C | returns B's data, and so on |

Once this starts, every request returns the previous request's answer for as
long as the replies have the same shape: wrong data shown as good. With
different shapes the same shift produces a string of `bad unit id` /
`protocol error` results that never clears on its own, because each failed
exchange leaves the real reply behind for the next one.

The same code runs for RTU over TCP and RTU over UDP (same transport, a socket
instead of a serial port), so they have the same defect.

## Root cause

`rtuTransport.ExecuteRequest` assumes the receive buffer is empty when it
transmits and never establishes that. There is no flush before the write and
no flush after a timeout.

## The fix

All in the RTU transport and the serial port wrapper.

1. **Flush before every transmit.** `ExecuteRequest` empties the link's
   receive buffer immediately before `Write` (after the t3.5 wait, so the
   window between "buffer empty" and "request on the wire" is as small as it
   can be). Whatever was received before the request went out cannot be its
   answer. A discarded-bytes count is logged as a warning.

   This also covers "resync after a timeout or a mismatch": whatever the
   previous exchange left behind (late reply, half a frame, the real reply
   after a mismatched one) is gone before the next request is sent.

2. **The flush must not cost bus time.** The existing `discard()` reads with a
   500 us deadline, but on a serial port `serialPortWrapper.Read` blocks in
   `goburrow/serial`'s `select()` for its fixed 10 ms timeout when the buffer
   is empty. 10 ms in front of every request is more than a whole exchange at
   115200 baud, so the transport does not use `discard()` on serial ports.

   Instead the transport asks the link whether it can flush without waiting
   (new unexported interface `rxFlusher`, one method `flushRx() int`).
   `serialPortWrapper` implements it:

   - POSIX: `goburrow/serial` opens the descriptor with `O_NDELAY` and never
     clears it (`serial_posix.go:53`, "VMIN/VTIME unused as NDELAY is
     utilized"), so a plain `read()` on the descriptor returns `EAGAIN` at
     once when nothing is pending. `flushRx` reads until `EAGAIN`. That is one
     system call in the normal (empty) case. `goburrow/serial` has no flush
     call and keeps the descriptor in an unexported field, so the wrapper
     reads that field through `reflect` (read-only; no `unsafe`). If the field
     is ever not there, the wrapper falls back to the blocking drain below.
   - Windows / fallback: `port.Read` until it returns no data (one 10 ms wait
     when the buffer is empty). Correct, just slower; the gateway does not run
     there.

   Links that do not implement `rxFlusher` (sockets: RTU over TCP / UDP, and
   the `net.Pipe` used by the tests) keep using `discard()`: at most 500 us
   per request, which is noise next to a network round trip. Reading through
   the link also empties the UDP wrapper's left-over buffer.

3. **Skip a frame that cannot be the answer.** If the late reply arrives
   *after* the flush (while the new request is going out), it is still read
   first. When its unit id or function code does not match the request, the
   transport now drops it and keeps reading until the deadline, the way the
   TCP transport already skips foreign transaction ids
   (`tcp_transport.go` `readResponse`). If nothing matching arrives before the
   deadline, the mismatched frame is handed to the client exactly as before,
   so the caller still gets `ErrBadUnitId` / `ErrProtocolError` (only later).

4. The i/o deadline is now set after the flush (the fallback flush moves the
   link's deadline), i.e. immediately before the write instead of before the
   t3.5 wait.

### What the fix cannot do

A late reply that arrives after the flush **and** has the same unit id,
function code and length as the expected one is indistinguishable from the
real answer. That is a property of Modbus RTU (no transaction id), not of this
code. The real reply that follows it is discarded by the next request's flush,
so the damage is one wrong read, not a permanent shift. The only cure is a
response timeout longer than the slowest device on the bus.

## What does not change

- Inter-frame timing: the t3.5 wait before transmit, the "line busy" estimate
  after `Write`, and the wait before reading are untouched. On a serial port
  the flush is a non-blocking `read()`.
- `readRTUFrame`, CRC handling, `expectedResponseLenth`, the post-error
  "wait 256 character times and discard" path.
- The client: all unit id / function / length checks stay where they are and
  return the same errors.
- TCP / TLS / TCP-over-UDP transports (see below).
- Public API: nothing added, nothing removed. `rxFlusher` is unexported.

## Other transports (checked, not changed)

- **Modbus TCP, TCP over TLS**: fine. `tcpTransport.ExecuteRequest` increments
  `lastTxnId` per request and `readResponse` discards every frame whose
  transaction id differs (`tcp_transport.go:98-125`). A late reply to request
  N is read and skipped while waiting for N+1.
- **Modbus TCP over UDP**: transaction ids are checked the same way. Separate,
  smaller weakness (not fixed here): `udpSockWrapper` turns datagrams into a
  byte stream, so a truncated or garbled datagram shifts the MBAP framing of
  everything behind it and nothing resynchronises. It fails with errors, not
  with wrong data.
- **RTU over TCP, RTU over UDP**: same transport as serial, same defect, fixed
  by the same change (they take the `discard()` path).

## Compatibility

- Callers see no API change. Behaviour changes only when stale bytes are
  present: they are dropped instead of being parsed as the next answer.
- A wrong-unit / wrong-function frame now costs up to the configured timeout
  before the same error is returned, because the transport waits for the real
  answer first.
- The request deadline starts at transmit instead of before the t3.5 wait
  (at most a few ms later).
- New log line (warning) when stale bytes are discarded.

## Release

The gateway vendors this module (`v1.1.2`, plus an old `v1.1.0` copy under
`hapimb/vendor`). The same change is applied by hand to both vendored copies
on the gateway branch `fix/rtu-flush-late-replies` so the gateway builds with
it today. Still to do by someone who can push:

1. merge this branch, tag a release (`v1.1.3`);
2. in MDI-Gateway: `go get github.com/Altronic-LLC/altronic_modbus@v1.1.3`
   and `go mod vendor`.

Until then **do not run `go mod vendor` in MDI-Gateway**: it would restore the
unfixed v1.1.2 files. The gateway's PTY test added with this fix fails if that
happens.

## Tests

`rtu_flush_test.go` (in-memory fake serial line whose receive buffer behaves
like the kernel's tty buffer):

- `TestRTULateReplyIsNotTheNextAnswer`: reply arrives after the timeout, the
  next request has the same unit / function / quantity. Must return the new
  reply and leave nothing in the buffer.
- `TestRTULateReplyDoesNotShiftOtherUnits`: same late reply, the next requests
  go to units 2, 1, 2. All must succeed with their own data.
- `TestRTUSkipsFrameOfAnotherRequest`: a frame of another unit (and a read
  reply in front of a write reply) arrives after the flush, followed by the
  real answer: the real answer is returned. When only the mismatched frame
  arrives, the caller still gets `ErrBadUnitId` / `ErrProtocolError`.
- `TestRTUFlushesThroughTheLinkBeforeWriting`: a link implementing `rxFlusher`
  is flushed through it once per request, before the write, and `discard()`
  is not used (no second deadline update).

`serial_flush_posix_test.go`:

- `TestSerialPortFdIsReachable`: the descriptor field is found on a
  `goburrow/serial` port of the version in `go.mod`.
- `TestDrainFd`: a non-blocking descriptor is read dry (1500 bytes, more than
  one read) and an empty one returns at once.

The real serial path (pty opened through `goburrow/serial`, real
`serialPortWrapper`) is covered in MDI-Gateway, which already has a PTY
harness: `rtu_late_reply_test.go`.

## Results

Run on macOS arm64, Go 1.26.5.

Old code (`main`), new tests only:

```
--- FAIL: TestRTULateReplyIsNotTheNextAnswer
    request #2 returned the late reply to request #1: 0xaaaa 0xaaaa
    9 bytes left in the rx buffer after request #2
--- FAIL: TestRTULateReplyDoesNotShiftOtherUnits
    request #2 (unit 2) should have succeeded, got: bad unit id
--- FAIL: TestRTUSkipsFrameOfAnotherRequest
    request should have succeeded, got: bad unit id
--- FAIL: TestRTUFlushesThroughTheLinkBeforeWriting
    request #1 returned stale data: 02 aa aa
    expected 3 flushes (one per request), saw 0
```

(`serial_flush_posix_test.go` does not compile against the old code: the
functions it tests are new.)

New code: all six new tests pass, also with `-race -count=5`. The package
builds for darwin, linux/arm64 and windows.

Whole package, `go test .`: the same four tests fail before and after this
change, all with `x509: certificate contains duplicate extension` from the
test certificates under Go 1.26 (`TestTCPoverTLSClient`,
`TestTLSClientOnServerTimeout`, `TestTLSServer`, `TestServerExtractRole`).
Not related, not touched. `go vet ./...` / `go test ./...` also fail before and
after for `cmd/` and `examples/`, which still import
`github.com/simonvetter/modbus`. `go vet .` reports the same three
"unreachable code" findings as before (`server.go`, `rtu_transport_test.go`).

Gateway, real serial path over a PTY at 115200 baud
(`TestRTULateReplyIsDiscarded`):

| vendored library | request after a late reply | average exchange |
|---|---|---|
| v1.1.2 (old) | returns the late reply's registers, no error | - |
| this fix | correct registers, `discarded 9 stale byte(s)` logged | 4.8 ms |
| this fix, non-blocking flush disabled by hand | correct registers | 17.0 ms |

The last row is why the serial flush goes to the descriptor instead of
through `port.Read`.

Not done here: no run against real RS-485 hardware.
