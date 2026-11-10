package main

import "testing"

func TestDeleteLaterCmdLine(t *testing.T) {
	got := deleteLaterCmdLine(`C:\tools\jellyfin\ffagent.exe`, "")
	want := `cmd.exe /d /s /c "for /l %i in (1,1,30) do @(ping -n 2 127.0.0.1 >nul & del /f /q "C:\tools\jellyfin\ffagent.exe" >nul 2>&1 & if not exist "C:\tools\jellyfin\ffagent.exe" (exit /b 0))"`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	withDir := deleteLaterCmdLine(`C:\x y\ffagent\ffagent.exe`, `C:\x y\ffagent`)
	wantDir := `cmd.exe /d /s /c "for /l %i in (1,1,30) do @(ping -n 2 127.0.0.1 >nul & del /f /q "C:\x y\ffagent\ffagent.exe" >nul 2>&1 & if not exist "C:\x y\ffagent\ffagent.exe" (rmdir "C:\x y\ffagent" >nul 2>&1 & exit /b 0))"`
	if withDir != wantDir {
		t.Fatalf("got\n%s\nwant\n%s", withDir, wantDir)
	}
}
