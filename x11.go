package main

// Minimal pure-Go X11 client (stdlib only) with just enough of the XKB
// extension to watch and set the keyboard layout group.
//
// Wire formats verified against the XCB protocol definitions
// (xorg/proto/xcbproto: src/xproto.xml, src/xkb.xml).

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	xkbUseCoreKbd     = 0x100 // DeviceSpec::UseCoreKbd
	xkbEvStateNotify  = 2     // XKB event number (relative to firstEvent)
	xkbEvStateBit     = 0x04  // EventType::StateNotify
	xkbStateGroupPart = 0x10  // StatePart::GroupState
)

type respMsg struct {
	seq  uint16
	data []byte // full reply: 32-byte header + length*4
	err  error
}

type xkbStateEvent struct {
	group   byte
	changed uint16
}

type X11 struct {
	conn          net.Conn
	writeMu       sync.Mutex
	seq           uint32
	xkbOpcode     byte
	xkbFirstEvent byte
	responses     chan respMsg
	events        chan xkbStateEvent
}

func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }

// parseDisplay parses an X display string: ":0", ":0.0", "host:1", "@host:0", "unix:0".
func parseDisplay(d string) (network, address string, err error) {
	host, disp := "", d
	if i := strings.IndexByte(d, ':'); i >= 0 {
		host, disp = d[:i], d[i+1:]
	}
	if strings.HasPrefix(host, "@") || host == "unix" {
		host = ""
	}
	if i := strings.IndexByte(disp, '.'); i >= 0 {
		disp = disp[:i]
	}
	n, err := strconv.Atoi(disp)
	if err != nil {
		return "", "", fmt.Errorf("bad display %q", d)
	}
	if host == "" {
		return "unix", fmt.Sprintf("/tmp/.X11-unix/X%d", n), nil
	}
	return "tcp", fmt.Sprintf("%s:%d", host, 6000+n), nil
}

// connectX connects to the X server, first without auth, then with the
// MIT-MAGIC-COOKIE-1 from the X authority file.
func connectX(display string) (*X11, error) {
	if display == "" {
		display = ":0"
	}
	x, err := tryConnect(display, "", nil)
	if err == nil {
		return x, nil
	}
	if name, data, ok := readXAuthority(display); ok {
		x2, err2 := tryConnect(display, name, data)
		if err2 == nil {
			return x2, nil
		}
		return nil, fmt.Errorf("X display %s: %v (with auth: %v)", display, err, err2)
	}
	return nil, fmt.Errorf("X display %s: %v", display, err)
}

// debugConn wraps a conn and hex-dumps all traffic when X11_DEBUG is set.
type debugConn struct {
	net.Conn
	mu sync.Mutex
}

func (d *debugConn) Write(p []byte) (int, error) {
	if os.Getenv("X11_DEBUG") != "" {
		d.mu.Lock()
		fmt.Fprintf(os.Stderr, ">> %s\n", hex.EncodeToString(p))
		d.mu.Unlock()
	}
	return d.Conn.Write(p)
}

func (d *debugConn) Read(p []byte) (int, error) {
	n, err := d.Conn.Read(p)
	if os.Getenv("X11_DEBUG") != "" && n > 0 {
		d.mu.Lock()
		fmt.Fprintf(os.Stderr, "<< %s\n", hex.EncodeToString(p[:n]))
		d.mu.Unlock()
	}
	return n, err
}

func maybeDebug(c net.Conn) net.Conn {
	if os.Getenv("X11_DEBUG") != "" {
		return &debugConn{Conn: c}
	}
	return c
}

func tryConnect(display, authName string, authData []byte) (*X11, error) {
	network, address, err := parseDisplay(display)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout(network, address, 3*time.Second)
	if err != nil {
		return nil, err
	}
	conn = maybeDebug(conn)

	// Initial message (SetupRequest):
	// byte_order(1) pad(1) major(2) minor(2) name_len(2) data_len(2) pad(2)
	// name(pad4) data(pad4)
	buf := []byte{'B', 0, 0, 11, 0, 0}
	buf = append(buf, byte(len(authName)>>8), byte(len(authName)))
	buf = append(buf, byte(len(authData)>>8), byte(len(authData)))
	buf = append(buf, 0, 0)
	buf = append(buf, authName...)
	for len(buf)%4 != 0 {
		buf = append(buf, 0)
	}
	buf = append(buf, authData...)
	for len(buf)%4 != 0 {
		buf = append(buf, 0)
	}
	if _, err := conn.Write(buf); err != nil {
		conn.Close()
		return nil, err
	}

	hdr := make([]byte, 8)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		conn.Close()
		return nil, err
	}
	// Setup responses have an 8-byte header (not 32):
	//   Failed:       status(1) reason_len(1) major(2) minor(2) length(2) reason
	//   Authenticate: status(1) pad(5) length(2) reason
	//   Success:      status(1) pad(1) major(2) minor(2) length(2) data
	switch hdr[0] {
	case 0: // Failed
		reason := make([]byte, int(hdr[1]))
		io.ReadFull(conn, reason)
		conn.Close()
		return nil, fmt.Errorf("connection failed: %s", strings.TrimSpace(string(reason)))
	case 2: // Authenticate
		n := int(be16(hdr[6:8])) * 4
		reason := make([]byte, n)
		io.ReadFull(conn, reason)
		conn.Close()
		return nil, fmt.Errorf("auth required: %s", strings.TrimSpace(string(reason)))
	case 1: // Success
	default:
		conn.Close()
		return nil, fmt.Errorf("unexpected setup status %d", hdr[0])
	}
	if n := int(be16(hdr[6:8])) * 4; n > 0 {
		rest := make([]byte, n)
		if _, err := io.ReadFull(conn, rest); err != nil {
			conn.Close()
			return nil, err
		}
	}

	x := &X11{
		conn:      conn,
		responses: make(chan respMsg, 16),
		events:    make(chan xkbStateEvent, 64),
	}
	go x.readLoop()
	return x, nil
}

// readLoop is the single reader of the socket: it dispatches replies to
// responses and XKB StateNotify events to events.
func (x *X11) readLoop() {
	hdr := make([]byte, 32)
	for {
		if _, err := io.ReadFull(x.conn, hdr); err != nil {
			x.responses <- respMsg{err: err}
			return
		}
		t := hdr[0]
		seq := be16(hdr[2:4])
		if t == 1 { // only replies carry a length field; events/errors are 32 bytes
			length := int(be16(hdr[4:6])) * 4
			if length > 0 {
				rest := make([]byte, length)
				if _, err := io.ReadFull(x.conn, rest); err != nil {
					x.responses <- respMsg{err: err}
					return
				}
				hdr = append(hdr, rest...)
			}
		}
		switch {
		case t == 0: // error
			x.responses <- respMsg{seq: seq, err: fmt.Errorf("X error %d (seq %d)", hdr[1], seq)}
		case t == 1: // normal reply
			x.responses <- respMsg{seq: seq, data: hdr}
		case x.xkbFirstEvent != 0 && t == x.xkbFirstEvent+xkbEvStateNotify:
			// xkbStateNotify (32 bytes): type[0] xkbType[1] seq[2:4] time[4:8]
			// deviceID[8] mods[9] baseMods[10] latchedMods[11] lockedMods[12]
			// group[13] baseGroup[14:16] latchedGroup[16:18] lockedGroup[18]
			// compatState[19] ... ptrBtnState[24:26] changed[26:28] keycode[28:30]
			x.events <- xkbStateEvent{group: hdr[13], changed: be16(hdr[26:28])}
		}
		// all other types are events we do not care about
	}
}

// sendRequest writes one request (header: opcode, dataByte, length; then
// fields) and returns the sequence number used. It does not wait for a
// reply.
func (x *X11) sendRequest(op, dataByte byte, fields []byte) (uint16, error) {
	padded := len(fields)
	for padded%4 != 0 {
		padded++
	}
	total := 4 + padded
	var seq uint16
	x.writeMu.Lock()
	x.seq++
	seq = uint16(x.seq)
	l := total / 4 // length field is in 4-byte units, includes header
	req := make([]byte, 0, total)
	req = append(req, op, dataByte, byte(l>>8), byte(l))
	req = append(req, fields...)
	for len(req) < total {
		req = append(req, 0)
	}
	_, werr := x.conn.Write(req)
	x.writeMu.Unlock()
	return seq, werr
}

// request sends one request and waits for its reply.
func (x *X11) request(op, dataByte byte, fields []byte) ([]byte, error) {
	seq, err := x.sendRequest(op, dataByte, fields)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case r := <-x.responses:
			if r.err != nil {
				return nil, r.err
			}
			if r.seq == seq {
				return r.data, nil
			}
		case <-time.After(time.Until(deadline)):
			return nil, fmt.Errorf("timeout waiting for X reply")
		}
	}
}

// sendNoReply sends a request that produces no reply (XkbSelectEvents,
// XkbLatchLockState, ...). The server still assigns it a sequence number,
// but never sends a response for it.
func (x *X11) sendNoReply(op, dataByte byte, fields []byte) error {
	_, err := x.sendRequest(op, dataByte, fields)
	return err
}

// queryExtension asks for an extension's opcode and first event/error.
func (x *X11) queryExtension(name string) (opcode, firstEvent, firstError byte, err error) {
	fields := make([]byte, 4+len(name))
	binary.BigEndian.PutUint16(fields[0:2], uint16(len(name)))
	copy(fields[4:], name)
	for len(fields)%4 != 0 {
		fields = append(fields, 0)
	}
	rep, err := x.request(98, 0, fields) // X_QueryExtension
	if err != nil {
		return 0, 0, 0, err
	}
	if os.Getenv("X11_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "QueryExtension reply: %x\n", rep)
	}
	// Reply layout: present at [8], major_opcode at [9], first_event at [10],
	// first_error at [11] (verified against live server + setxkbmap strace).
	if len(rep) < 32 || rep[8] == 0 {
		return 0, 0, 0, fmt.Errorf("extension %q not present", name)
	}
	return rep[9], rep[10], rep[11], nil
}

// initXKB queries the XKEYBOARD extension and enables it.
func (x *X11) initXKB() error {
	op, firstEvent, _, err := x.queryExtension("XKEYBOARD")
	if err != nil {
		return err
	}
	x.xkbOpcode = op
	x.xkbFirstEvent = firstEvent
	if err := x.useExtension(); err != nil {
		return err
	}
	return nil
}

// useExtension enables the XKB extension (XkbUseExtension, minor 0).
// Must be sent before any other XKB request, otherwise the server
// returns BadMatch/BadExtension.
func (x *X11) useExtension() error {
	fields := make([]byte, 4)
	binary.BigEndian.PutUint16(fields[0:2], 1) // wantedMajor
	binary.BigEndian.PutUint16(fields[2:4], 0) // wantedMinor
	_, err := x.request(x.xkbOpcode, 0, fields)
	return err
}

// getState returns the current keyboard group (XkbGetState, minor 4).
// Reply data (after 32-byte header): deviceID[6] mods[7] baseMods[8]
// latchedMods[9] lockedMods[10] group[11] lockedGroup[12] baseGroup[13:15]
// latchedGroup[15:17] compatState[17] grabMods[18] compatGrabMods[19]
// lookupMods[20] compatLookupMods[21] pad[22] ptrBtnState[23:25] pad[25:31]
func (x *X11) getState() (byte, error) {
	fields := make([]byte, 4)
	binary.BigEndian.PutUint16(fields[0:2], xkbUseCoreKbd)
	rep, err := x.request(x.xkbOpcode, 4, fields)
	if err != nil {
		return 0, err
	}
	if len(rep) < 13 {
		return 0, fmt.Errorf("short XkbGetState reply")
	}
	return rep[12], nil
}

// selectGroupEvents selects XkbStateNotify events for group changes
// (XkbSelectEvents, minor 1; selection is per-client).
func (x *X11) selectGroupEvents() error {
	fields := make([]byte, 16)
	binary.BigEndian.PutUint16(fields[0:2], xkbUseCoreKbd)
	binary.BigEndian.PutUint16(fields[2:4], xkbEvStateBit) // affectWhich = StateNotify
	// clear=0, selectAll=0, affectMap=0, map=0
	binary.BigEndian.PutUint16(fields[12:14], xkbStateGroupPart) // affectState
	binary.BigEndian.PutUint16(fields[14:16], xkbStateGroupPart) // stateDetails
	err := x.sendNoReply(x.xkbOpcode, 1, fields)
	return err
}

// setGroup locks the keyboard to the given group (XkbLatchLockState, minor 5,
// lockGroup=True — the X11 way to programmatically set the layout group,
// same as Xlib's XkbLockGroup).
func (x *X11) setGroup(group byte) error {
	// LatchLockState (12 field bytes): deviceSpec(2) affectModLocks(1)
	// modLocks(1) lockGroup(1) groupLock(1) affectModLatches(1) pad(1)
	// pad(1) latchGroup(1) groupLatch(2)
	fields := make([]byte, 12)
	binary.BigEndian.PutUint16(fields[0:2], xkbUseCoreKbd)
	fields[2] = 0 // affectModLocks
	fields[3] = 0 // modLocks
	fields[4] = 1 // lockGroup = True
	fields[5] = group
	fields[6] = 0 // affectModLatches
	// fields[7]=pad, fields[8]=pad, fields[9]=latchGroup=0, fields[10:12]=groupLatch=0
	err := x.sendNoReply(x.xkbOpcode, 5, fields)
	return err
}

// clearGroupLock resets the group lock (XkbLatchLockState, minor 5,
// lockGroup=True, groupLock=0). The XKB effective group is the sum of the
// locked+base+latched offsets, so setting locked_group=0 returns the
// keyboard to its base group. Called when the host connection is lost, so
// the guest stops being pinned to the last synced group.
func (x *X11) clearGroupLock() error {
	fields := make([]byte, 12)
	binary.BigEndian.PutUint16(fields[0:2], xkbUseCoreKbd)
	fields[2] = 0 // affectModLocks
	fields[3] = 0 // modLocks
	fields[4] = 1 // lockGroup = True
	fields[5] = 0 // groupLock = 0 (clear the lock offset)
	fields[6] = 0 // affectModLatches
	return x.sendNoReply(x.xkbOpcode, 5, fields)
}

func (x *X11) Close() {
	x.conn.Close()
}
