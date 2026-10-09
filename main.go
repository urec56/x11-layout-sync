package main

// x11-layout-sync — a sway-layout-sync analog for X11 (bspwm, i3, ...).
//
// Streams keyboard layout (XKB group) changes from the host to the guest
// over plain TCP, applied via XKB requests (sub-millisecond, no SPICE
// modifier desync).
//
// Usage:
//
//	host:  x11-layout-sync -s [addr]     (server mode, default 192.168.122.1:8889)
//	guest: x11-layout-sync [addr]        (client mode)
//
//	x11-layout-sync -q                  just print the local X11 layout group

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultAddr = "192.168.122.1:8889"

// ---------------------------------------------------------------
// SERVER MODE (runs on the HOST)
// ---------------------------------------------------------------

type Server struct {
	mu            sync.Mutex
	clients       map[net.Conn]struct{}
	currentGroup  byte
}

func (s *Server) broadcast(g byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentGroup = g
	fmt.Printf("[Host] Layout group %d — broadcasting to %d client(s)\n", g, len(s.clients))
	for c := range s.clients {
		_, _ = fmt.Fprintf(c, "%d\n", g)
	}
}

func runServer(addr string) {
	display := os.Getenv("DISPLAY")
	x, err := connectX(display)
	if err != nil {
		fmt.Printf("[Host Error] %v\n", err)
		os.Exit(1)
	}
	defer x.Close()
	if err := x.initXKB(); err != nil {
		fmt.Printf("[Host Error] %v\n", err)
		os.Exit(1)
	}
	if err := x.selectGroupEvents(); err != nil {
		fmt.Printf("[Host Error] Failed to select XKB group events: %v\n", err)
		os.Exit(1)
	}
	g, err := x.getState()
	if err != nil {
		fmt.Printf("[Host Error] Failed to read XKB state: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[Host] Watching X11 display %s (group %d)\n", display, g)

	srv := &Server{
		clients:      make(map[net.Conn]struct{}),
		currentGroup: g,
	}

	l, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Printf("[Host Error] Failed to listen on %s: %v\n", addr, err)
		os.Exit(1)
	}
	defer l.Close()
	fmt.Printf("[Host] Listening on %s, waiting for clients...\n", addr)

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			fmt.Printf("[Host] Client connected: %s\n", conn.RemoteAddr())
			srv.mu.Lock()
			srv.clients[conn] = struct{}{}
			_, _ = fmt.Fprintf(conn, "%d\n", srv.currentGroup)
			srv.mu.Unlock()

			go func(c net.Conn) {
				buf := make([]byte, 1)
				for {
					if _, err := c.Read(buf); err != nil {
						break
					}
				}
				srv.mu.Lock()
				delete(srv.clients, c)
				srv.mu.Unlock()
				c.Close()
				fmt.Printf("[Host] Client disconnected: %s\n", conn.RemoteAddr())
			}(conn)
		}
	}()

	// Event-driven group watching, with a slow polling fallback in case
	// some group changes don't produce StateNotify events.
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case ev := <-x.events:
			if ev.changed&xkbStateGroupPart != 0 && ev.group != srv.currentGroup {
				srv.broadcast(ev.group)
			}
		case <-ticker.C:
			g, err := x.getState()
			if err == nil && g != srv.currentGroup {
				srv.broadcast(g)
			}
		}
	}
}

// ---------------------------------------------------------------
// CLIENT MODE (runs on the GUEST)
// ---------------------------------------------------------------

func runClient(addr string) {
	display := os.Getenv("DISPLAY")
	x, err := connectX(display)
	if err != nil {
		fmt.Printf("[Guest Error] %v\n", err)
		os.Exit(1)
	}
	defer x.Close()
	if err := x.initXKB(); err != nil {
		fmt.Printf("[Guest Error] %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[Guest] Connected to X11 display %s\n", display)

	// Reconnection loop.
	for {
		fmt.Printf("[Guest] Connecting to host at %s...\n", addr)
		tcpConn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		fmt.Println("[Guest] Connected to host")

		scanner := bufio.NewScanner(tcpConn)
		for scanner.Scan() {
			text := strings.TrimSpace(scanner.Text())
			if text == "" {
				continue
			}
			idx, err := strconv.Atoi(text)
			if err != nil || idx < 0 {
				continue
			}
			if err := x.setGroup(byte(idx)); err != nil {
				fmt.Printf("[Guest Error] Failed to set group %d: %v\n", idx, err)
				continue
			}
			fmt.Printf("[Guest] Applied layout group %d\n", idx)
		}

		// Connection lost: clear the group lock so the guest keyboard is no
		// longer pinned to the last synced group (returns to base group).
		if err := x.clearGroupLock(); err != nil {
			fmt.Printf("[Guest] Failed to clear group lock: %v\n", err)
		} else {
			fmt.Println("[Guest] Cleared group lock (connection lost)")
		}
		fmt.Println("[Guest] Connection lost, reconnecting...")
		tcpConn.Close()
		time.Sleep(1 * time.Second)
	}
}

// ---------------------------------------------------------------

func main() {
	serverMode := flag.Bool("s", false, "server mode (run on host)")
	query := flag.Bool("q", false, "print local X11 layout group and exit")
	flag.Parse()

	addr := flag.Arg(0)
	if addr == "" {
		addr = defaultAddr
	}

	if *query {
		x, err := connectX(os.Getenv("DISPLAY"))
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		defer x.Close()
		if err := x.initXKB(); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		g, err := x.getState()
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("layout group: %d\n", g)
		return
	}

	if *serverMode {
		runServer(addr)
	} else {
		runClient(addr)
	}
}
