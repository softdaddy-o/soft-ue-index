//go:build windows

package proc

import "os/exec"

type flagsOf struct{ *exec.Cmd }

func (f flagsOf) flags() uint32 { return f.SysProcAttr.CreationFlags }
