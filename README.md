# x11-layout-sync

A lightweight, sub-millisecond keyboard layout (XKB group) synchronizer between a host machine and a QEMU/KVM virtual machine running **X11** (`bspwm`, `i3`, `Openbox`, ...).

## Why?

When running X11 on both the host and a guest VM, switching the keyboard layout normally has to be done **twice** — once on the host and once inside the guest. `x11-layout-sync` removes that: layout (XKB group) changes on the host are streamed to the guest over a local TCP socket in **< 0.5 ms** and applied directly via XKB requests, so the guest always follows the host.

## How it works

- **Host (server mode)** connects to its X11 display, selects XKB `StateNotify` events, and watches the current layout group. On every change it broadcasts the group index to all connected clients.
- **Guest (client mode)** connects to its own X11 display and to the host over TCP. On every received group it applies it via `XkbLatchLockState` (locking the group while the connection is alive). When the connection drops, it clears the lock so the guest keyboard returns to normal local switching.

Both sides speak the standard X11/XKB protocol only — no extra dependencies, and the guest needs nothing but this single binary.

## Requirements

1. **Identical layout configurations**: the host and the guest must have the **same XKB layouts in the same order**, e.g. `setxkbmap -layout us,ru` on both.
2. The guest must be able to reach the host over TCP (see the firewall note below).

## Installation

### Option 1: Nix Flakes

Add to your system `flake.nix` inputs:

```nix
x11-layout-sync = {
  url = "github:urec56/x11-layout-sync";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

Add `x11-layout-sync` to your `environment.systemPackages`:

```nix
environment.systemPackages = [
  inputs.x11-layout-sync.packages.${pkgs.stdenv.hostPlatform.system}.default
];
```

### Option 2: Manual build

Requirements: `Go 1.20+` (standard library only, zero external dependencies).

```bash
git clone https://github.com/urec56/x11-layout-sync.git
cd x11-layout-sync
go build -o x11-layout-sync .
```

## Usage

1. **Start on the Host (server mode):**
   ```bash
   x11-layout-sync -s 192.168.122.1:8889
   ```

2. **Start in the Guest (client mode):**
   ```bash
   x11-layout-sync 192.168.122.1:8889
   ```

3. **Just print the local layout group:**
   ```bash
   x11-layout-sync -q
   ```

> The default address is `192.168.122.1:8889` (the QEMU/KVM NAT bridge). Override it with any `host:port`.

## Firewall (NixOS / Linux)

Make sure TCP port `8889` is open on the host's virtual bridge interface (`virbr0`):

```nix
networking.firewall.interfaces."virbr0".allowedTCPPorts = [ 8889 ];
```

## Disclaimer

This software is provided "as is", without warranty of any kind, express or implied. It communicates over unencrypted TCP sockets and is strictly intended for use within isolated, local virtual networks (such as QEMU's `192.168.122.0/24` bridge) between a host and its guest VM.
