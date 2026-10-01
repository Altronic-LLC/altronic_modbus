# RTU master: a late reply which arrives after the next request went out

Date: 2026-10-01
Branch: `fix/rtu-late-reply-quarantine` (from `main` b4e518b)
Follows: `docs/2026-10-01-rtu-flush-late-replies.md` (the first half)

## What happens today

The first half made the RTU master empty its receive buffer immediately before
every transmit (`rtu_transport.go:83`, `flushRx`) and skip a frame whose unit
id or function code cannot belong to the request (`:109-121`, `isResponseTo`).
That removes a late reply which is already in the buffer when the next request
is sent. It does nothing for a late reply which arrives *after* that:

- `rtu_transport.go:137-139`: after `ErrRequestTimedOut` the transport returns
  at once. It keeps no record that a reply to that request may still be on its
  way.
- `:73-95`: the next request waits t3.5, flushes, and is written. Nothing ties
  the moment it may be sent to the request which just timed out.
- `:109-113`: the first well-formed frame with the request's unit id and
  function code ends the wait. `client.go:1124-1145` then checks the byte
  count. A reply to the *previous* request to the same unit, with the same
  function and quantity, passes all of that.

Timeline (timeout 250 ms, slave answers 330 ms after each request), two
same-shaped reads sent back to back:

| t (ms) | wire | master |
|-------:|------|--------|
| 0 | request A (unit 33, FC3, reg 0, qty 2) | waits |
| 250 | - | A fails: `request timed out` |
| 252 | request B (unit 33, FC3, reg 10, qty 2) | flush (nothing to drop), waits |
| 330 | reply A arrives | CRC ok, unit ok, function ok, length ok |
| 330 | - | **B returns A's registers, no error** |
| 582 | reply B arrives | dropped by the next request's flush |

Reproduced on the lab unit on 2026-10-01 (38400 8N1, port timeout 250 ms,
slave 80 ms late): the second of two ranges read back to back showed the first
range's data, HTTP 200.

## Root cause

Modbus RTU has no transaction id. A reply belongs to a request only because of
*when* it arrives: nothing else was owed on the line when the request went out.
After a timeout that is not true - the unit still owes a reply - and the
transport does not know it: it forgets the timed out request and lets the next
one to the same unit go out while that reply can still arrive.

## The fix

All in `rtu_transport.go`. The transport remembers, per unit id, the replies
which timed out requests may still produce, and does not let such a reply be
taken for the answer to a later request.

### State (per unit id, created on that unit's first timeout)

- `owed`: number of timed out requests whose reply may still arrive;
- `owedUntil`: when the last of them is given up: time of the timeout + the
  **settle window** `S`;
- `misses`: requests in a row for which no frame at all came back from the
  unit.

`S = min(timeout, 1 s)`. A reply is therefore waited for, in total, up to
`timeout + S` after its request: twice the configured timeout, and never more
than one second past it. A reply later than that is the limit which remains
(see below).

### Rules

1. **A request which times out leaves a debt.** `owed++`,
   `owedUntil = now + S`, `misses++`.
2. **Every well-formed frame is booked against the unit which sent it**, where
   ever it is read (as an answer, while waiting for another unit's answer, or
   during rule 3): `misses = 0`, and if that unit has an unexpired debt the
   frame is a late reply: `owed--` and the frame is dropped. It is never
   returned as an answer, even when unit id, function and length match.
3. **Quarantine: a request to a unit with an unexpired debt is held back.**
   Before transmitting, the transport listens on the line until the debt is
   paid (the late frame arrives - the usual case for a slow device, so the wait
   ends early) or until `owedUntil`. Only then is the request sent. A frame
   whose header arrived inside the window is read to its end (up to one maximum
   frame time, 256 character times), so the window never ends in the middle of
   a late frame. Garbage during the wait is discarded the way a bad frame
   already is (wait one maximum frame time, flush) and the wait continues.
   Requests to *other* units are never held back: a late frame from unit A
   which lands in unit B's exchange is told apart by its unit id (and now pays
   A's debt as well).
4. **An absent unit is not waited for.** Once a unit has missed more than
   `rtuAbsentAfter` (2) requests in a row, rule 3 is skipped for it: the
   request is sent at once. Rule 2 still applies, so if the unit does come
   back with a late reply after the next request has gone out, that frame pays
   the debt and is dropped instead of being returned; the request then ends
   with the real answer if it follows in time, or with `ErrRequestTimedOut`.
   The first frame heard from the unit resets `misses`, which puts it back
   under rule 3.

Rule 3 is what makes a slow device correct *and* cheap: the master waits only
as long as the late reply takes to arrive. Rule 4 is what keeps a dead device
from costing bus time: at most two settle windows per outage, then nothing.

### Order inside `ExecuteRequest`

settle the unit (rule 3) -> wait t3.5 -> flush -> set deadline -> write ->
read frames (rule 2 on each) -> on timeout, book the debt (rule 1).

### Cost

Character time `t1 = 11 bits / baud`; maximum frame time `256 x t1`:

| baud | t1 | max frame time |
|---:|---:|---:|
| 9600 | 1.15 ms | 293 ms |
| 19200 | 0.57 ms | 147 ms |
| 38400 | 0.29 ms | 73 ms |
| 115200 | 0.095 ms | 24 ms |

- **Device answers in time:** nothing. No debt, no state touched beyond one map
  lookup per frame.
- **Slow device (answers after the timeout, within `timeout + S`):** the next
  request to it leaves when its late reply has been read, i.e. after the
  lateness, not after `S`. At 250 ms / 80 ms late: +80 ms.
- **One lost request (noise), device healthy:** the next request *to that
  unit* is held for up to `S` (250 ms at the default gateway timeout) if it is
  sent straight away; not at all if at least `S` has passed (poll delay, other
  devices' turns).
- **Absent device:** a request to it costs `timeout`, as today. Extra: the
  second and third request of an outage are each held for what is left of `S`
  at the moment they are issued - only if they are issued to the same unit
  inside the window. From the third consecutive miss on: zero. So an outage
  costs at most `2 x S` once (0.5 s at 250 ms, never more than 2 s), then
  exactly what it costs today.
- **Other devices on the port:** a request to another unit is never delayed by
  rule 3 itself. They can only be delayed if the caller chooses to issue a
  second request to the quarantined unit while they are waiting, and then by at
  most `S`, at most twice per outage of that unit (rule 4).
- The frame-in-flight allowance (one maximum frame time) is only spent when a
  frame header has actually arrived at the end of the window.

Poll rate with a dead unit, timeout 250 ms, caller asking it back to back
forever: today 4 requests/s on that unit. With the fix: requests 2 and 3 of the
outage leave 250 ms later each, then 4 requests/s again.

### What the fix still cannot do

A reply that arrives more than `S` after its request timed out (more than
`2 x timeout`, or `timeout + 1 s`) and after a later request of the same shape
has been sent is still indistinguishable. No bounded wait can close that. The
cure stays the same: a port timeout longer than the slowest device.

On rule 4's path, when the unit's earlier request was truly lost and it then
answers a back-to-back request promptly, that answer is dropped as a possible
late reply and the request reports a timeout: one extra failed request for a
unit which had already failed three times in a row. Wrong direction of error
on purpose: a failure, not wrong data.

## What does not change

- The pre-transmit flush, the unit/function skip, t3.5 handling, CRC and
  frame parsing, the post-error "wait 256 character times and discard".
- The client (`client.go`), all error values, the public API.
- Modbus TCP / TLS / TCP over UDP: they match replies by transaction id.
- The serial wrapper and the flush helpers.
- RTU over TCP / UDP use the same transport and get the same rules.

## Compatibility

- No API change. A request to a unit which has just timed out may be sent up to
  `S` later than before (rule 3); callers see a longer call, not a new error.
- A frame from a unit with a pending debt is no longer handed to the client as
  a mismatch; if nothing else arrives the caller gets `ErrRequestTimedOut`
  instead of `ErrBadUnitId` / `ErrProtocolError` for that case.
- New log line (warning): `dropped a late reply from unit N, function 0xNN`.

## Release

The gateway vendors this module (v1.1.2 under `vendor/`, v1.1.0 under
`hapimb/vendor`). The same change is applied by hand to both vendored copies on
the gateway branch `fix/rtu-late-reply-quarantine`. Still to do by someone who
can push: merge this branch, **tag a release** (`v1.1.3`, carrying both
halves), then in MDI-Gateway
`go get github.com/Altronic-LLC/altronic_modbus@v1.1.3 && go mod vendor`
(a **go.mod bump**). Until then do not run `go mod vendor` there: it restores
the unfixed v1.1.2 files.

## Tests

`rtu_quarantine_test.go`, on the fake serial line of `rtu_flush_test.go`. The
late frame is delivered a fixed time after request #1 was written, which on
the old code is after request #2 was written.

- `TestRTULateReplyAfterNextRequestIsNotItsAnswer`: slave answers every
  request late (after the timeout). Two same-shaped reads back to back: the
  second must not return the first's registers.
- `TestRTULateReplyThenPromptReply`: reply #1 is late, the slave answers #2 at
  once. Request #2 must return its own registers.
- `TestRTULateReplyCutByTheSettleWindow`: the late frame's header arrives just
  before the window ends, the rest after: it is consumed whole and request #2
  returns its own registers.
- `TestRTUAbsentUnitLateReply`: a unit which missed four requests (absent),
  then answers late, after the next request was written: that request must not
  return the late registers.
- `TestRTUAbsentUnitIsNotWaitedFor`: six back-to-back requests to a silent
  unit take six timeouts plus at most two settle windows; from the fourth on
  the request is written without delay.
- `TestRTUQuarantineDoesNotHoldOtherUnits`: after unit 1 timed out, a request
  to unit 2 is written at once and succeeds, and unit 1's late frame landing in
  it is skipped.

## Results

macOS arm64, Go 1.26.5. Not run on real RS-485 hardware in this step.

Old code (`main` b4e518b), new tests only - all six fail:

```
--- FAIL: TestRTULateReplyAfterNextRequestIsNotItsAnswer
    request #2 returned the late reply to request #1: 0x1111 0x1111
--- FAIL: TestRTULateReplyThenPromptReply
    request #2 returned the late reply to request #1: 0xaaaa 0xaaaa
    9 bytes left in the rx buffer after request #2
--- FAIL: TestRTULateReplyCutByTheSettleWindow
    request #2 should have succeeded, got: request timed out
--- FAIL: TestRTUAbsentUnitLateReply
    request #6 returned the late reply to request #5: 0x5555 0x5555
--- FAIL: TestRTUAbsentUnitIsNotWaitedFor
    request #2 was written 50.7ms after request #1, expected it to be held for the settle window (100ms)
    request #3 was written 58.3ms after request #2, expected it to be held for the settle window (100ms)
--- FAIL: TestRTUQuarantineDoesNotHoldOtherUnits
    request #3 returned the late reply to request #1: 0xaaaa 0xaaaa
```

New code: all six pass, and so do the first half's tests
(`rtu_flush_test.go`, `serial_flush_posix_test.go`) and the existing RTU
tests; `-race -count=10` on all of them passes. Checked by hand that the tests
watch the right thing: with the frame-in-flight allowance disabled
`TestRTULateReplyCutByTheSettleWindow` fails (`protocol error`); with the
absent rule disabled `TestRTUAbsentUnitIsNotWaitedFor` fails (requests 4-6
held 100 ms each).

Whole package, `go test .`: the same four tests fail before and after, all
with `x509: certificate contains duplicate extension` from the test
certificates under Go 1.26 (`TestTCPoverTLSClient`,
`TestTLSClientOnServerTimeout`, `TestTLSServer`, `TestServerExtractRole`). Not
related, not touched. `go vet .` reports the same three "unreachable code"
findings as before. `go vet ./...` / `go test ./...` still fail for `cmd/` and
`examples/` (they import `github.com/simonvetter/modbus`), as before. The
package builds for darwin, linux/arm64 and windows.

Gateway, real serial path over a PTY at 115200 baud, timeout 1 s, slave 200 ms
late on the first request (`TestRTULateReplyAfterNextRequest`):

| vendored library | request B, asked for right after A timed out |
|---|---|
| main | returns A's registers, no error |
| this fix | returns its own registers; it was sent 201 ms after it was asked for (when the late reply was in), not after the 1 s window |
