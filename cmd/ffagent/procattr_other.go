//go:build !linux && !windows

package main

import "os/exec"

func setProcAttr(*exec.Cmd) {}
