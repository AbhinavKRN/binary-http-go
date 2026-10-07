# BHT/1 — a binary HTTP

Version 1. Wire identifier `BHT1`.

BHT/1 carries HTTP-shaped request/response exchanges over a single, long-lived
TCP connection, in binary. It is self-delimiting: every message boundary is
computed from declared lengths, never from a connection close.

The key words MUST, MUST NOT, SHOULD, MAY are to be interpreted as in RFC 2119.
An implementation is conformant if it satisfies every MUST in this document and
rejects every input in the "must reject" list at the end. Rationale for the
choices made here is in [README.md](README.md); this document is normative.

## 1. Connection

1.1. BHT/1 runs over TCP. The default port is 9000, but the port carries no
meaning.

1.2. Immediately after the connection is established, the client MUST send the
4-byte **preface** `42 48 54 31` (`"BHT1"`). It is sent exactly once, before
any frame, and is not itself a frame.

1.3. The server MUST NOT send any byte before it has received the complete
preface. If the first 4 bytes received are not the preface, the server MUST
send one ERROR frame (Section 6) with status 400 on stream 0 and then close the
connection.

1.4. The connection is persistent. A server MUST NOT close the connection
merely because it has finished a response. Either endpoint MAY close an idle
connection; a server that intends to close after a particular response MUST
include a `connection: close` header in it.

1.5. All multi-byte integers in this protocol are unsigned and big-endian.

## 2. Frame layout

Every byte after the preface belongs to a frame. A frame is an 8-byte header
followed by exactly `Length` bytes of payload.

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                    Length (24)                |    Type (8)   |
+---------------+---------------+-------------------------------+
|    Flags (8)  | Reserved (8)  |         Stream ID (16)        |
+---------------+---------------+-------------------------------+
|                    Payload (Length bytes)                   ...
+---------------------------------------------------------------+
```

| Field | Bytes | Meaning |
|---|---|---|
| Length | 0-2 | Payload size in bytes, excluding this header |
| Type | 3 | Frame type (Section 3) |
| Flags | 4 | Bit field (Section 2.3) |
| Reserved | 5 | MUST be `0x00` on send; MUST be ignored on receipt |
| Stream ID | 6-7 | Exchange this frame belongs to (Section 4) |

2.1. `Length` MUST NOT exceed **65535** (`MAX_FRAME_SIZE`). A receiver that
reads a larger `Length` MUST send ERROR 400 on stream 0 and close the
connection; it MUST NOT attempt to buffer the payload.

2.2. A receiver MUST NOT act on a frame until all `Length` payload bytes have
arrived, and MUST consume exactly `Length` bytes. Byte `Length + 1` belongs to
the next frame.

2.3. Flags:

| Bit | Name | Meaning |
|---|---|---|
| `0x01` | END_MESSAGE | This is the final frame of its message |

Bits `0x02`–`0x80` are unassigned. A sender MUST set them to 0. **A receiver
MUST ignore flag bits it does not recognise** — an unknown flag is not an
error.

## 3. Frame types

| Code | Name | Payload |
|---|---|---|
| `0x01` | REQUEST | Section 5.1 |
| `0x02` | RESPONSE | Section 5.2 |
| `0x03` | DATA | Opaque message body bytes |
| `0x04` | ERROR | Section 6 |
| `0x05`–`0xFF` | *unassigned* | — |

3.1. **A receiver that meets a frame type it does not know MUST skip it
cleanly**: read the 8-byte header, read and discard exactly `Length` payload
bytes, and continue with the next frame on the same connection. It MUST NOT
close the connection, MUST NOT send an error, and MUST NOT let the frame affect
any stream's state.

This is the entire forward-compatibility mechanism of BHT/1 and the reason
`Length` precedes `Type` in the header: a receiver can always determine how
much to skip without understanding what it is skipping.

3.2. A REQUEST frame MUST be sent only by a client; a RESPONSE frame only by a
server. Violations are a 400.

## 4. Streams

4.1. A **stream** is one request and its response. Stream ID `0` refers to the
connection itself and is used only by ERROR frames.

4.2. Streams are initiated by the client and MUST use **odd** IDs: 1, 3, 5, …
Each new REQUEST MUST use an ID strictly greater than every ID previously used
on that connection. A server receiving a reused or non-increasing ID MUST
answer ERROR 400 on stream 0 and close.

4.3. Even non-zero IDs are reserved for a future server-initiated mechanism. A
server receiving an even stream ID MUST answer 400.

4.4. A client that would need an ID above 65535 MUST close the connection and
open a new one. IDs MUST NOT wrap.

4.5. A client MAY send several requests before reading any response
(pipelining). A server MAY answer in any order; the client matches responses to
requests by stream ID, not by arrival order. Frames belonging to different
streams MAY be interleaved.

## 5. Messages

A message is one REQUEST or RESPONSE frame, followed by zero or more DATA
frames on the same stream. The final frame of a message MUST set END_MESSAGE. A
message with no body is a single frame with END_MESSAGE set.

5.1. **REQUEST payload**

```
+--------+----------+------------------+-------------+------------------+
| Method | PathLen  | Path             | HeaderCount | Header entries   |
| u8     | u16      | PathLen bytes    | u8          | HeaderCount of   |
+--------+----------+------------------+-------------+------------------+
```

| Method | Code |
|---|---|
| GET | `0x01` |
| HEAD | `0x02` |
| POST | `0x03` |
| PUT | `0x04` |
| DELETE | `0x05` |
| OPTIONS | `0x06` |
| PATCH | `0x07` |

Codes `0x00` and `0x08`–`0xFF` are unassigned; a receiver MUST answer 400. A
method that is known but not permitted for the target is 405, not 400.

`Path` is an origin-form target in US-ASCII: it MUST begin with `/`, MUST NOT be
empty, MUST NOT exceed 8192 bytes, and MUST NOT contain bytes below `0x21` or
above `0x7E`. Percent-encoding is as in RFC 3986. A path that still contains a
`.` or `..` segment after percent-decoding MUST be answered with 400; servers
MUST NOT resolve it.

5.2. **RESPONSE payload**

```
+--------+-------------+------------------+
| Status | HeaderCount | Header entries   |
| u16    | u8          | HeaderCount of   |
+--------+-------------+------------------+
```

`Status` is an HTTP status code as an integer (200, not `"200"`).

5.3. **Header entry**

```
+------------+   index form (first byte has the high bit set)
| 0x80|index |   index is 1..10, from the static table below
+------------+
+------+---------+---------+   literal form (first byte is 0x00)
| 0x00 | NameLen | Name    |   NameLen: u8
+------+---------+---------+
followed in both forms by:
+----------+---------+
| ValueLen | Value   |   ValueLen: u16
| u16      | bytes   |
+----------+---------+
```

Static table:

| Index | Name | Index | Name |
|---|---|---|---|
| 1 | `content-length` | 6 | `connection` |
| 2 | `content-type` | 7 | `user-agent` |
| 3 | `host` | 8 | `accept` |
| 4 | `date` | 9 | `last-modified` |
| 5 | `server` | 10 | `cache-control` |

A first byte of `0x01`–`0x7F` is unassigned (reserved for a future dynamic
table) and MUST be answered with 400. `0x80` (index 0) and indices above 10
MUST be answered with 400.

Literal names MUST be lowercase, non-empty, and composed of RFC 9110 token
characters. Values MUST NOT contain bytes below `0x20` or equal to `0x7F`.
`HeaderCount` MUST match the number of entries actually present; trailing bytes
in the payload, or a payload that ends mid-entry, are a 400.

5.4. **Bodies.** The body of a message is the concatenation of the payloads of
its DATA frames, in order. There is no chunked encoding and no need for one:
each frame already declares its own length, and END_MESSAGE declares the end of
the sequence.

5.5. If a `content-length` header is present it MUST equal the total number of
body bytes delivered. A receiver MUST answer 400 on mismatch. Framing comes
from the frames; `content-length` is metadata that is checked, not trusted.

5.6. A response to a HEAD request MUST carry the `content-length` the body
would have had and MUST NOT contain any DATA frame. A client MUST NOT apply
the 5.5 check to such a response.

## 6. Errors

6.1. An **ERROR frame** (`0x04`) reports a failure that cannot be expressed as a
response to a stream. Payload:

```
+--------+-----------+------------------+
| Status | ReasonLen | Reason (UTF-8)   |
| u16    | u16       | ReasonLen bytes  |
+--------+-----------+------------------+
```

6.2. An ERROR frame on stream 0 is connection-level: the sender MUST close the
connection immediately after it, and the receiver MUST NOT send anything
further.

6.3. A failure that can be attributed to a stream SHOULD be reported as a normal
RESPONSE frame with the appropriate status, leaving the connection usable.

6.4. Status codes used by this protocol:

| Status | Meaning in BHT/1 |
|---|---|
| 200 | Success |
| 400 | The frame or message violated this specification |
| 404 | No such resource |
| 405 | Method is valid but not permitted for this resource |
| 413 | Body exceeds what the receiver accepts |
| 500 | Receiver fault |

## 7. Limits

| Limit | Value |
|---|---|
| `MAX_FRAME_SIZE` (payload) | 65535 bytes |
| Maximum path length | 8192 bytes |
| Maximum header entries per message | 255 (implied by `HeaderCount: u8`) |
| Maximum stream ID | 65535 |

A receiver MAY enforce lower limits, and MUST report doing so with 400 (or 413
for a body) rather than by closing silently.

## 8. Worked example

`GET /index.html` with two headers, as a single frame with no body:

```
00 00 2c 01 01 00 00 01     header: len=44 type=REQUEST flags=END_MESSAGE
                            reserved=0 stream=1
01                          method GET
00 0b                       path length 11
2f 69 6e 64 65 78 2e        "/index.html"
68 74 6d 6c
02                          2 header entries
83                          static index 3 -> host
00 0e                       value length 14
6c 6f 63 61 6c 68 6f 73     "localhost:9000"
74 3a 39 30 30 30
87                          static index 7 -> user-agent
00 09                       value length 9
62 63 75 72 6c 2f 31 2e 30  "bcurl/1.0"
```

A fully annotated request *and* response, captured from a live exchange, is in
[HEXDUMP.md](HEXDUMP.md).

## 9. Conformance: inputs an implementation must reject

A conformant receiver answers 400 (and, where noted, closes) for each of these.
Machine-readable vectors for all of them are in
[`tests/vectors/invalid.jsonl`](tests/vectors/invalid.jsonl); a foreign
implementation can be checked against the same file.

1. A first 4 bytes that are not `BHT1` — *close*.
2. `Length` greater than `MAX_FRAME_SIZE` — *close*.
3. A REQUEST frame on an even or reused or non-increasing stream ID — *close*.
4. A REQUEST frame on stream 0 — *close*.
5. Method code `0x00` or above `0x07`.
6. A path that is empty, does not start with `/`, exceeds 8192 bytes, contains a
   byte outside `0x21`–`0x7E`, or contains a `.` or `..` segment after decoding.
7. A header entry whose first byte is `0x01`–`0x7F`, or `0x80`, or an index
   above 10.
8. A literal header name that is empty, uppercase, or not a token.
9. A payload that ends mid-field, or has bytes left over after `HeaderCount`
   entries.
10. A `content-length` that disagrees with the delivered body length.
11. A DATA frame on a stream that has already seen END_MESSAGE, or on a stream
    that was never opened.
12. A RESPONSE frame received by a server, or a REQUEST frame received by a
    client.

And the one input a conformant receiver must **not** reject:

13. A frame whose type is in `0x05`–`0xFF`. It is skipped, and the connection
    carries on.
