package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"

	"zeolite/internal/sandbox"
)

// ifreq is the kernel's struct ifreq: a device's name and one setting.
type ifreq struct {
	name [unix.IFNAMSIZ]byte
	data [24]byte
}

func named() ifreq {
	var r ifreq
	copy(r.name[:], sandbox.TUNDevice)
	return r
}

func ioctl(fd int, request uint, arg unsafe.Pointer, what string) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(request), uintptr(arg)); errno != 0 {
		return fmt.Errorf("%s: %w", what, errno)
	}
	return nil
}

// sockaddr writes an IPv4 address as the kernel's struct sockaddr_in.
func sockaddr(into []byte, addr [4]byte) {
	*(*uint16)(unsafe.Pointer(&into[0])) = unix.AF_INET
	copy(into[4:8], addr[:])
}

// rtentry is the kernel's struct rtentry, as SIOCADDRT takes it.
type rtentry struct {
	_       uint64
	dst     [16]byte
	gateway [16]byte
	genmask [16]byte
	flags   uint16
	_       int16
	_       uint64
	_       uintptr
	metric  int16
	dev     *byte
	mtu     uint64
	window  uint64
	irtt    uint16
}

// openDevice makes the sandbox's TUN device in this namespace, gives it
// the sandbox's address, brings it up, and sends everything the
// namespace sends to it. It uses the kernel's own calls and no other
// program.
func openDevice() (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening /dev/net/tun: %w", err)
	}
	create := named()
	*(*uint16)(unsafe.Pointer(&create.data[0])) = unix.IFF_TUN | unix.IFF_NO_PI
	if err := ioctl(fd, unix.TUNSETIFF, unsafe.Pointer(&create), "making the device"); err != nil {
		return nil, err
	}
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("a socket to configure the device with: %w", err)
	}
	defer unix.Close(sock)

	address := named()
	sockaddr(address.data[:], sandbox.SandboxAddress.As4())
	mask := named()
	sockaddr(mask.data[:], [4]byte{255, 255, 255, 255})
	mtu := named()
	*(*int32)(unsafe.Pointer(&mtu.data[0])) = sandbox.PacketMTU
	flags := named()
	name := named()
	route := rtentry{flags: unix.RTF_UP, dev: &name.name[0]}
	sockaddr(route.dst[:], [4]byte{})
	sockaddr(route.genmask[:], [4]byte{})
	for _, step := range []struct {
		request uint
		arg     unsafe.Pointer
		what    string
		before  func()
	}{
		{unix.SIOCSIFADDR, unsafe.Pointer(&address), "giving the device its address", nil},
		{unix.SIOCSIFNETMASK, unsafe.Pointer(&mask), "giving the device its mask", nil},
		{unix.SIOCSIFMTU, unsafe.Pointer(&mtu), "setting the device's MTU", nil},
		{unix.SIOCGIFFLAGS, unsafe.Pointer(&flags), "reading the device's flags", nil},
		{unix.SIOCSIFFLAGS, unsafe.Pointer(&flags), "bringing the device up", func() {
			*(*uint16)(unsafe.Pointer(&flags.data[0])) |= unix.IFF_UP
		}},
		{unix.SIOCADDRT, unsafe.Pointer(&route), "routing the namespace to the device", nil},
	} {
		if step.before != nil {
			step.before()
		}
		if err := ioctl(sock, step.request, step.arg, step.what); err != nil {
			return nil, err
		}
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "/dev/net/tun"), nil
}
