// ttyrun starts a child in a new process group to show how that interacts with
// a controlling terminal. Mode "plain" leaves the child in the background
// group; mode "fg" hands it the terminal.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func main() {
	cmd := exec.Command("sh", "-c", `read x; echo "child-read:$x"`)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	attr := &syscall.SysProcAttr{Setpgid: true}
	if os.Args[1] == "fg" {
		attr = &syscall.SysProcAttr{Setpgid: true, Foreground: true, Ctty: 0}
	}
	cmd.SysProcAttr = attr
	signal.Ignore(syscall.SIGTTOU)
	err := cmd.Run()
	fmt.Println("wrapper-done", err)
}
