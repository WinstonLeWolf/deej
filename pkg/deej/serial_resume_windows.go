//go:build windows

package deej

import (
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pGetModuleHandle                     = kernel32.NewProc("GetModuleHandleW")
	pRegisterClassEx                     = user32.NewProc("RegisterClassExW")
	pUnregisterClass                     = user32.NewProc("UnregisterClassW")
	pCreateWindowEx                      = user32.NewProc("CreateWindowExW")
	pDestroyWindow                       = user32.NewProc("DestroyWindow")
	pDefWindowProc                       = user32.NewProc("DefWindowProcW")
	pGetMessage                          = user32.NewProc("GetMessageW")
	pTranslateMessage                    = user32.NewProc("TranslateMessage")
	pDispatchMessage                     = user32.NewProc("DispatchMessageW")
	pPostMessage                         = user32.NewProc("PostMessageW")
	pPostQuitMessage                     = user32.NewProc("PostQuitMessage")
	pRegisterSuspendResumeNotification   = user32.NewProc("RegisterSuspendResumeNotification")
	pUnregisterSuspendResumeNotification = user32.NewProc("UnregisterSuspendResumeNotification")
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

// setupResumeListener creates a hidden top-level window and runs a Win32 message loop
// to catch WM_POWERBROADCAST (sleep/hibernate/resume) and WM_DEVICECHANGE (USB plug/unplug)
// events. When the system wakes or a USB device arrives/leaves, it triggers NotifyResume()
// so the serial manager immediately drops stale handles and reconnects.
func (sio *SerialIO) setupResumeListener() {
	var once sync.Once
	ready := make(chan struct{})

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		hInst, _, _ := pGetModuleHandle.Call(0)
		classNamePtr, err := windows.UTF16PtrFromString("deej_power_listener_window")
		if err != nil {
			sio.logger.Warnw("Failed to convert power listener class name", "error", err)
			once.Do(func() { close(ready) })
			return
		}
		windowNamePtr, _ := windows.UTF16PtrFromString("deej_power_listener")

		wndProc := func(hWnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
			const (
				WM_POWERBROADCAST = 0x0218
				WM_DEVICECHANGE   = 0x0219
				WM_CLOSE          = 0x0010
				WM_DESTROY        = 0x0002

				PBT_APMSUSPEND         = 0x0004
				PBT_APMRESUMEAUTOMATIC = 0x0012
				PBT_APMRESUMESUSPEND   = 0x0007

				DBT_DEVICEARRIVAL        = 0x8000
				DBT_DEVICEREMOVECOMPLETE = 0x8004
			)

			switch msg {
			case WM_POWERBROADCAST:
				switch wParam {
				case PBT_APMRESUMEAUTOMATIC, PBT_APMRESUMESUSPEND:
					sio.logger.Infow("Windows power resume event detected, triggering reconnect", "wParam", wParam)
					sio.NotifyResume()
				case PBT_APMSUSPEND:
					sio.logger.Info("Windows power suspend event detected, closing serial connection proactively")
					sio.NotifyResume()
				}
			case WM_DEVICECHANGE:
				switch wParam {
				case DBT_DEVICEARRIVAL:
					sio.logger.Debug("Device arrival event detected, notifying serial reconnect")
					sio.NotifyResume()
				case DBT_DEVICEREMOVECOMPLETE:
					sio.logger.Debug("Device removal event detected, notifying serial reconnect")
					sio.NotifyResume()
				}
			case WM_CLOSE:
				pDestroyWindow.Call(uintptr(hWnd))
				return 0
			case WM_DESTROY:
				pPostQuitMessage.Call(0)
				return 0
			}

			ret, _, _ := pDefWindowProc.Call(uintptr(hWnd), uintptr(msg), wParam, lParam)
			return ret
		}

		wcex := wndClassEx{
			Size:      uint32(unsafe.Sizeof(wndClassEx{})),
			WndProc:   windows.NewCallback(wndProc),
			Instance:  windows.Handle(hInst),
			ClassName: classNamePtr,
		}

		res, _, err := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wcex)))
		if res == 0 {
			sio.logger.Warnw("Failed to register power listener window class", "error", err)
			once.Do(func() { close(ready) })
			return
		}
		defer pUnregisterClass.Call(uintptr(unsafe.Pointer(classNamePtr)), uintptr(windows.Handle(hInst)))

		// Top-level unowned window (style=0, parent=0) to receive broadcast messages
		hWnd, _, err := pCreateWindowEx.Call(
			0,
			uintptr(unsafe.Pointer(classNamePtr)),
			uintptr(unsafe.Pointer(windowNamePtr)),
			0,
			0, 0, 0, 0,
			0, 0,
			uintptr(windows.Handle(hInst)),
			0,
		)
		if hWnd == 0 {
			sio.logger.Warnw("Failed to create power listener window", "error", err)
			once.Do(func() { close(ready) })
			return
		}

		// Also register for Modern Standby suspend/resume notifications if supported
		if pRegisterSuspendResumeNotification.Find() == nil {
			const DEVICE_NOTIFY_WINDOW_HANDLE = 0
			hNotify, _, _ := pRegisterSuspendResumeNotification.Call(hWnd, DEVICE_NOTIFY_WINDOW_HANDLE)
			if hNotify != 0 && pUnregisterSuspendResumeNotification.Find() == nil {
				defer pUnregisterSuspendResumeNotification.Call(hNotify)
			}
		}

		sio.logger.Debug("Windows power & device listener initialized successfully")
		once.Do(func() { close(ready) })

		var msg struct {
			HWnd    windows.Handle
			Message uint32
			WParam  uintptr
			LParam  uintptr
			Time    uint32
			Pt      struct{ X, Y int32 }
		}

		for {
			r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if int32(r) <= 0 {
				break
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			pDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
		}
	}()

	<-ready
}
