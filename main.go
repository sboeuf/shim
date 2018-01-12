// Copyright 2017 HyperHQ Inc.
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"flag"
	"fmt"
	"log/syslog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"unsafe"

	"github.com/sirupsen/logrus"
	lSyslog "github.com/sirupsen/logrus/hooks/syslog"
	"golang.org/x/sys/unix"
)

const (
	shimName    = "kata-shim"
	exitFailure = 1
)

// version is the shim version. This variable is populated at build time.
var version = "unknown"

var shimLog = logrus.New()

//var shimLog = logrus.WithFields(logrus.Fields{
//	"name": shimName,
//	"pid":  os.Getpid(),
//})

func initLogger(logLevel string) error {
//	shimLog.Logger.Formatter = &logrus.TextFormatter{TimestampFormat: time.RFC3339Nano}

	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		return err
	}

	logrus.SetLevel(level)

	shimLog.WithField("version", version).Info()

	return nil
}

func main() {
	var (
		logLevel      string
		agentAddr     string
		container     string
		execId        string
		proxyExitCode bool
		showVersion   bool
	)

	flag.BoolVar(&showVersion, "version", false, "display program version and exit")
	flag.StringVar(&logLevel, "log", "debug", "set shim log level: debug, info, warn, error, fatal or panic")
	flag.StringVar(&agentAddr, "agent", "", "agent gRPC socket endpoint")

	flag.StringVar(&container, "container", "", "container id for the shim")
	flag.StringVar(&execId, "exec-id", "", "process id for the shim")
	flag.BoolVar(&proxyExitCode, "proxy-exit-code", true, "proxy exit code of the process")

	flag.Parse()

	if showVersion {
		fmt.Printf("%v version %v\n", shimName, version)
		os.Exit(0)
	}

	if agentAddr == "" || container == "" || execId == "" {
		shimLog.WithField("agentAddr", agentAddr).WithField("container", container).WithField("exec-id", execId).Error("container ID, exec ID and agent socket endpoint must be set")
		os.Exit(exitFailure)
	}

	hook, err := lSyslog.NewSyslogHook("", "", syslog.LOG_DEBUG, "")
	if err == nil {
		shimLog.Hooks.Add(hook)
	}

	if err := initLogger(logLevel); err != nil {
		shimLog.WithError(err).WithField("loglevel", logLevel).Error("invalid log level")
		os.Exit(exitFailure)
	}

	shimLog.Info("### SHIM STARTED")

	// Wait on SIGUSR1 if it is an init process shim, in which case
	// container ID equals to exec ID.
	if container == execId {
		waitSigUsr1 := make(chan os.Signal, 1)
		signal.Notify(waitSigUsr1, syscall.SIGUSR1)
		<-waitSigUsr1
		signal.Stop(waitSigUsr1)
	}

	shimLog.Info("### SHIM 1")

	shim, err := newShim(agentAddr, container, execId)
	if err != nil {
		shimLog.WithError(err).Error("failed to create new shim")
		os.Exit(exitFailure)
	}
	shimLog.Info("### SHIM 2")

	// stdio
	wg := &sync.WaitGroup{}
	shim.proxyStdio(wg)
	defer wg.Wait()
	shimLog.Info("### SHIM 3")

	// winsize
	termios, err := setupTerminal()
	if err != nil {
		shimLog.WithError(err).Error("failed to set raw terminal")
		os.Exit(exitFailure)
	}
	defer restoreTerminal(termios)
	shim.monitorTtySize(os.Stdin)
	shimLog.Info("### SHIM 4")

	// signals
	sigc := shim.forwardAllSignals()
	defer signal.Stop(sigc)

	shimLog.Info("### SHIM 5")
	// wait until exit
	exitcode, err := shim.wait()
	if err != nil {
		shimLog.Info("### SHIM 6")
		shimLog.WithError(err).WithField("exec-id", execId).Error("failed waiting for process")
		os.Exit(exitFailure)
	} else if proxyExitCode {
		shimLog.Info("### SHIM 7")
		shimLog.WithField("exitcode", exitcode).Info("using shim to proxy exit code")
		if exitcode != 0 {
			os.Exit(int(exitcode))
		}
	}

	shimLog.Info("### SHIM 8")
}

func isTerminal(fd uintptr) bool {
	var termios syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&termios)))

	return err == 0
}

func setupTerminal() (*unix.Termios, error) {
	if !isTerminal(os.Stdin.Fd()) {
		return nil, nil
	}

	var termios unix.Termios
	var savedTermios unix.Termios

	if _, _, err := unix.Syscall(unix.SYS_IOCTL, os.Stdin.Fd(), unix.TCGETS, uintptr(unsafe.Pointer(&termios))); err != 0 {
		return nil, fmt.Errorf("Could not get tty info: %s", err.Error())
	}

	savedTermios = termios

	// Set the terminal in raw mode
	termios.Iflag &^= (unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON)
	termios.Oflag &^= unix.OPOST
	termios.Lflag &^= (unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN)
	termios.Cflag &^= (unix.CSIZE | unix.PARENB)
	termios.Cflag |= unix.CS8
	termios.Cc[unix.VMIN] = 1
	termios.Cc[unix.VTIME] = 0

	if _, _, err := unix.Syscall(unix.SYS_IOCTL, os.Stdin.Fd(), unix.TCSETS, uintptr(unsafe.Pointer(&termios))); err != 0 {
		return nil, fmt.Errorf("Could not set tty in raw mode: %s", err.Error())
	}

	return &savedTermios, nil
}

func restoreTerminal(termios *unix.Termios) error {
	if !isTerminal(os.Stdin.Fd()) {
		return nil
	}

	if _, _, err := unix.Syscall(unix.SYS_IOCTL, os.Stdin.Fd(), unix.TCSETS, uintptr(unsafe.Pointer(termios))); err != 0 {
		return fmt.Errorf("Could not restore tty settings: %s", err.Error())
	}

	return nil
}

