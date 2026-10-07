# Specification notes

Notes from implementing BHT/1 in Go using only [`spec/SPEC.md`](spec/SPEC.md)
and the vectors in [`spec/vectors/`](spec/vectors/). Where the spec was tested
against another implementation, that code is referred to as the Python
reference.

Each item names the test that demonstrates it.

## Defects found in the Python reference (interop)

### 1. HEAD error responses include a body

SPEC 5.6 requires that a response to HEAD must not contain DATA frames. The
Python server omits the body for successful HEAD responses but attaches one for
error responses (for example `HEAD /missing.txt` → 404 head + 30-byte DATA).

Go `bcurl` rejects this and exits 8. Python `bcurl` accepts it.

- Test: `interop/TestGoClientAgainstPythonServer/HEAD_of_a_missing_file`
- Proposed SPEC addition: clients that receive DATA on a HEAD stream should
  treat the response as malformed.

### 2. Connection closed without `connection: close`

SPEC 1.4 requires a server that will close after a response to include
`connection: close` in that response. The Python server closes after
10,000 requests without announcing it.

- Go server adds the header on the last allowed response.
- Test: `internal/server/TestRequestCapAnnouncesConnectionClose`

## Rules in the vectors but not in SPEC.md

### 3. Backslash and decoded control bytes in paths

The vectors reject `\` in paths and `%00` after decoding. SPEC 5.1 allows
`0x21`–`0x7E` on the wire, which includes `\`, and does not mention
percent-decoded control bytes.

Both implementations follow the vectors.

### 4. Even stream IDs

SPEC 4.3 says a server "MUST answer 400" for an even stream ID. Section 9 and
the vectors require closing the connection. Both implementations close.

## Ambiguities where implementations differ

### 5. Malformed percent-escapes

| Path | Python reference | Go server |
|---|---|---|
| `/%zz` | 404 | 400 |
| `/hello.txt%` | 404 | 400 |

Go rejects malformed `%` sequences before lookup. Python treats them as literal
characters in the filename.

- Test: `interop/TestWhereTheSpecIsSilent`

## Ambiguities where implementations agree (spec should say so)

### 6. Query string

Dot-segment and NUL rules apply only to the path before `?`. Both servers return
200 for `/hello.txt?x=/../y`.

- Test: `interop/TestWhereTheSpecIsSilent`

### 7. Stream ID consumed on malformed REQUEST

A REQUEST with an invalid payload still consumes its stream ID. A second REQUEST
on the same ID is rejected at connection level.

- Test: `interop/TestStreamStateAfterAStreamError`

### 8. DATA after a rejected head

When a REQUEST head fails but END_MESSAGE is not set, DATA on that stream is
drained until END_MESSAGE rather than killing the connection.

- Test: `interop/TestStreamStateAfterAStreamError`

## Implementation choices not specified

### 9. ERROR on a non-zero stream

Go treats ERROR on an open stream as a stream failure; ERROR on an unknown
stream is a connection error.

### 10. Message head size

REQUEST and RESPONSE heads must fit in one frame (max 65535 bytes). There is no
CONTINUATION frame in BHT/1 v1.

### 11. content-length syntax

Go accepts only decimal digits, rejects signs and spaces, and requires repeated
headers to agree.

- Test: `internal/wire/TestContentLength`
