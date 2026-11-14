//go:build windows

package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	wmAppTray       = 0x8001
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmRButtonUp     = 0x0205
	wmLButtonDblClk = 0x0203
	wmContextMenu   = 0x007B
	nimAdd          = 0
	nimDelete       = 2
	nifMessage      = 1
	nifIcon         = 2
	nifTip          = 4
	imageIcon       = 1
	lrLoadFromFile  = 0x0010
	tpmRightButton  = 0x0002
	tpmReturnCmd    = 0x0100
	mfString        = 0x0000
	mfSeparator     = 0x0800
	openMenuID      = 1001
	exitMenuID      = 1002
)

var (
	user32              = syscall.NewLazyDLL("user32.dll")
	shell32             = syscall.NewLazyDLL("shell32.dll")
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	registerClassEx     = user32.NewProc("RegisterClassExW")
	createWindowEx      = user32.NewProc("CreateWindowExW")
	destroyWindow       = user32.NewProc("DestroyWindow")
	defWindowProc       = user32.NewProc("DefWindowProcW")
	getMessage          = user32.NewProc("GetMessageW")
	translateMessage    = user32.NewProc("TranslateMessage")
	dispatchMessage     = user32.NewProc("DispatchMessageW")
	postQuitMessage     = user32.NewProc("PostQuitMessage")
	loadImage           = user32.NewProc("LoadImageW")
	loadIcon            = user32.NewProc("LoadIconW")
	destroyIcon         = user32.NewProc("DestroyIcon")
	createPopupMenu     = user32.NewProc("CreatePopupMenu")
	appendMenu          = user32.NewProc("AppendMenuW")
	destroyMenu         = user32.NewProc("DestroyMenu")
	getCursorPos        = user32.NewProc("GetCursorPos")
	setForegroundWindow = user32.NewProc("SetForegroundWindow")
	trackPopupMenu      = user32.NewProc("TrackPopupMenu")
	notifyIcon          = shell32.NewProc("Shell_NotifyIconW")
	getModuleHandle     = kernel32.NewProc("GetModuleHandleW")
	trayURL             string
	trayExit            func()
	trayNotify          trayNotification
)

type trayPoint struct{ X, Y int32 }
type trayMessage struct {
	HWND           uintptr
	Message        uint32
	_              uint32
	WParam, LParam uintptr
	Time           uint32
	Point          trayPoint
	Private        uint32
}
type trayClass struct {
	Size, Style                        uint32
	WndProc                            uintptr
	ClsExtra, WndExtra                 int32
	Instance, Icon, Cursor, Background uintptr
	MenuName, ClassName                *uint16
	SmallIcon                          uintptr
}
type trayNotification struct {
	Size                uint32
	HWND                uintptr
	ID, Flags, Callback uint32
	Icon                uintptr
	Tip                 [128]uint16
	State, StateMask    uint32
	Info                [256]uint16
	Version             uint32
	InfoTitle           [64]uint16
	InfoFlags           uint32
	GUID                [16]byte
	BalloonIcon         uintptr
}

func winStr(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

func showTrayMenu(hwnd uintptr) {
	menu, _, _ := createPopupMenu.Call()
	if menu == 0 {
		return
	}
	defer destroyMenu.Call(menu)
	appendMenu.Call(menu, mfString, openMenuID, uintptr(unsafe.Pointer(winStr("Open Ghost"))))
	appendMenu.Call(menu, mfSeparator, 0, 0)
	appendMenu.Call(menu, mfString, exitMenuID, uintptr(unsafe.Pointer(winStr("Exit Ghost"))))
	var point trayPoint
	getCursorPos.Call(uintptr(unsafe.Pointer(&point)))
	setForegroundWindow.Call(hwnd)
	choice, _, _ := trackPopupMenu.Call(menu, tpmRightButton|tpmReturnCmd, uintptr(point.X), uintptr(point.Y), 0, hwnd, 0)
	switch choice {
	case openMenuID:
		go openBrowser(trayURL)
	case exitMenuID:
		destroyWindow.Call(hwnd)
	}
}
func ghostWndProc(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr {
	switch msg {
	case wmAppTray:
		switch uint32(lp) {
		case wmLButtonDblClk:
			go openBrowser(trayURL)
		case wmRButtonUp, wmContextMenu:
			showTrayMenu(hwnd)
		}
		return 0
	case wmClose:
		destroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		notifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&trayNotify)))
		if trayExit != nil {
			go trayExit()
		}
		postQuitMessage.Call(0)
		return 0
	}
	result, _, _ := defWindowProc.Call(hwnd, uintptr(msg), wp, lp)
	return result
}
func startTray(url string, quit func()) {
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		trayURL, trayExit = url, quit
		className := winStr("GhostNotificationWindow")
		instance, _, _ := getModuleHandle.Call(0)
		cls := trayClass{Size: uint32(unsafe.Sizeof(trayClass{})), WndProc: syscall.NewCallback(ghostWndProc), Instance: instance, ClassName: className}
		atom, _, err := registerClassEx.Call(uintptr(unsafe.Pointer(&cls)))
		if atom == 0 {
			log.Printf("Tray registration failed: %v", err)
			return
		}
		hwnd, _, err := createWindowEx.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(winStr("Ghost"))), 0, 0, 0, 0, 0, 0, 0, instance, 0)
		if hwnd == 0 {
			log.Printf("Tray window failed: %v", err)
			return
		}
		// Prefer the file packaged beside the executable. Fall back to EXE resource 1.
		path, _ := os.Executable()
		iconFile := filepath.Join(filepath.Dir(path), "ghost.ico")
		icon, _, _ := loadImage.Call(0, uintptr(unsafe.Pointer(winStr(iconFile))), imageIcon, 32, 32, lrLoadFromFile)
		if icon == 0 {
			icon, _, _ = loadIcon.Call(instance, 1)
		}
		if icon == 0 {
			icon, _, _ = loadIcon.Call(0, 32512)
		}
		if icon != 0 {
			defer destroyIcon.Call(icon)
		}
		trayNotify = trayNotification{Size: uint32(unsafe.Sizeof(trayNotification{})), HWND: hwnd, ID: 1, Flags: nifMessage | nifIcon | nifTip, Callback: wmAppTray, Icon: icon}
		copy(trayNotify.Tip[:], syscall.StringToUTF16("Ghost — peer-to-peer messenger"))
		added, _, err := notifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&trayNotify)))
		if added == 0 {
			log.Printf("Tray icon failed: %v", err)
			destroyWindow.Call(hwnd)
			return
		}
		var message trayMessage
		for {
			code, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
			if int32(code) <= 0 {
				break
			}
			translateMessage.Call(uintptr(unsafe.Pointer(&message)))
			dispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
		}
	}()
}
