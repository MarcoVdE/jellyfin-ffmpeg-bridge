package main

import "fmt"

// deleteLaterCmdLine is the cmd.exe command line that deletes exe once this
// process has exited and released it (retrying for up to ~30 seconds), then
// removes dir if given and empty.
//
// /s makes cmd.exe strip only the outer quotes and keep the rest verbatim,
// so the quoted paths inside survive.
func deleteLaterCmdLine(exe, dir string) string {
	done := "exit /b 0"
	if dir != "" {
		done = fmt.Sprintf(`rmdir "%s" >nul 2>&1 & exit /b 0`, dir)
	}
	script := fmt.Sprintf(`for /l %%i in (1,1,30) do @(ping -n 2 127.0.0.1 >nul & del /f /q "%s" >nul 2>&1 & if not exist "%s" (%s))`,
		exe, exe, done)
	return `cmd.exe /d /s /c "` + script + `"`
}
