# Habikat

<img src="assets/logo.png" alt="Habikat logo" width="128">

Habikat aims to make running coding agents safer by giving them disposable local virtual machines, separate from your host environment.

> Habikat is in alpha and not ready for general use.

The current version supports creating VMs, opening a shell, fetching committed guest branches, and deleting VMs. Coding agents are not bundled yet, and guests have no internet access.

## Development

Build the VM image once:

```sh
make image
```

Build and try the CLI:

```sh
make build
./bin/hk create --name demo
./bin/hk attach demo
```

Exiting the shell leaves the VM running. To remove it and all its files:

```sh
./bin/hk delete demo
```

Run `make build` again after changing the code.

## Test

- `make test` — unit tests with race detection.
- `make fuzz` — all fuzz tests.
- `make e2e` — real VM lifecycle and terminal tests.

The end-to-end tests require working KVM and a built VM image.

## Requirements

- Linux x86_64 with read/write access to `/dev/kvm`.
- Go 1.27.4 or later, Make, and a C compiler for race tests.
- Python 3.11+, GitHub CLI (`gh`), `curl`, and GNU `tar` for building images.
- OpenSSH client, GNU `cp`, and `mkfs.ext4` from e2fsprogs.

## TODOs

- [ ] Review and harden isolation.
- [ ] Add coding agent setup and development tools.
- [ ] Provide controlled network and project access.
- [ ] Simplify installation and image updates.
- [ ] Expand automated testing and failure recovery.
