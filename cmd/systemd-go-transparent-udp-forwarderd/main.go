package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/activation"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

var lastReceived *time.Time

func main() {
	exitIdleTime := flag.Duration("exit-idle-time", 0, "Exit when without a connection for this duration")
	printUsage := flag.Bool("help", false, "Print help text and exit")
	bufSize := flag.Uint("buffer", 1024, "Buffer size in bytes")

	flag.Parse()
	if *printUsage {
		flag.Usage()
		return
	}

	if *bufSize == 0 {
		log.Fatal("buffer size cannot be zero")
	}

	targets := flag.Args()
	if len(targets) == 0 {
		log.Fatal("no targets provided")
	}

	lis, err := activation.PacketConns()
	if err != nil {
		log.Fatal("couldn't start listening to systemd sockets - launched outside systemd?, err:" + err.Error())
	}

	if len(lis) == 0 {
		log.Fatal("no systemd listeners provided")
	}

	{
		now := time.Now()
		lastReceived = &now
	}

	eg, ctx := errgroup.WithContext(context.Background())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if *exitIdleTime != 0 {
		go func() {
			for range time.Tick(time.Second * 10) {
				if lastReceived.Add(*exitIdleTime).Before(time.Now()) {
					log.Println("idle-time has been exceeded, exiting")
					cancel()
					return
				}
			}
		}()
	}

	addr0, err := net.ResolveUDPAddr("udp", targets[0])
	if err != nil {
		panic(err)
	}
	fmt.Println("server target: ", addr0.String())

	for _, in := range lis {
		eg.Go(func() error {
			return forwardLoop(ctx, *bufSize, in, addr0, *exitIdleTime)
		})
	}
	log.Println("loop started")

	err = eg.Wait()

	if err != nil {
		log.Fatal("error when trying to run the pipe, err: " + err.Error() + "\n")
	}

	log.Println("shutdown")
}

func forwardLoop(ctx context.Context, bufSz uint, in net.PacketConn, target *net.UDPAddr, timeout time.Duration) error {

	defer in.Close()

	now := time.Now()
	in.SetReadDeadline(now.Add(timeout))
	lastReceived = &now

	buf := make([]byte, bufSz)
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, src, err := in.ReadFrom(buf)
		if errors.Is(err, io.EOF) {
			return nil
		}

		now := time.Now()
		in.SetReadDeadline(now.Add(timeout))
		lastReceived = &now

		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil
		}
		if err != nil {
			log.Println("failed to read packets, err: " + err.Error())
			continue
		}
		srcUDP := src.(*net.UDPAddr)

		sock, err := getOrCreateTransSock(srcUDP)
		if err != nil {
			return errors.Wrap(err, "failed to get a transparent socket")
		}

		if _, err = sock.WriteTo(buf[0:n], target); err != nil {
			log.Printf("failed to forward packets err: %v", err)
		}

	}
	return nil
}

var socks = map[string]net.PacketConn{}
var mu sync.Mutex

func getOrCreateTransSock(src *net.UDPAddr) (net.PacketConn, error) {
	mu.Lock()
	defer mu.Unlock()
	if v, ok := socks[src.String()]; ok {
		return v, nil
	}

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, syscall.IPPROTO_UDP)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create an AF_INET socket")
	}

	if err = syscall.SetsockoptInt(fd, syscall.SOL_IP, syscall.IP_FREEBIND, 1); err != nil {
		return nil, errors.Wrap(err, "setsockopt IP_FREEBIND failed, missing CAP_NET_ADMIN?")
	}
	if err = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return nil, errors.Wrap(err, "setsockopt SO_REUSEADDR failed!")
	}
	if err = syscall.SetsockoptInt(fd, syscall.SOL_IP, syscall.IP_TRANSPARENT, 1); err != nil {
		return nil, errors.Wrap(err, "setsockopt IP_TRANSPARENT failed, missing CAP_NET_ADMIN?")

	}
	addr := &syscall.SockaddrInet4{Port: src.Port, Addr: src.AddrPort().Addr().As4()}
	fmt.Println(src, addr)
	if err = syscall.Bind(fd, addr); err != nil {
		return nil, errors.Wrap(err, "transparent bind failed")
	}
	file := os.NewFile(uintptr(fd), "sockaddr-src-"+src.String())

	sock, err := net.FilePacketConn(file)
	if err != nil {
		return nil, errors.Wrap(err, "filepacket conn failed")
	}

	socks[src.String()] = sock
	return sock, nil
}
