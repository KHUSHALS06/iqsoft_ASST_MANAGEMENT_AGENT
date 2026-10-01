//go:build windows

package main

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/kbinani/screenshot"
)

// ---- screen capture -------------------------------------------------

func captureScreenJPEG(quality int) ([]byte, error) {
	bounds := screenshot.GetDisplayBounds(0)
	img, err := screenshot.CaptureRect(bounds)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---- input injection (SendInput) -------------------------------------

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procSendInput        = user32.NewProc("SendInput")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procVkKeyScanW       = user32.NewProc("VkKeyScanW")
)

const (
	smCxScreen = 0
	smCyScreen = 1

	inputMouse    = 0
	inputKeyboard = 1

	mouseEventMove       = 0x0001
	mouseEventAbsolute   = 0x8000
	mouseEventLeftDown   = 0x0002
	mouseEventLeftUp     = 0x0004
	mouseEventRightDown  = 0x0008
	mouseEventRightUp    = 0x0010
	mouseEventMiddleDown = 0x0020
	mouseEventMiddleUp   = 0x0040
	mouseEventWheel      = 0x0800

	keyEventKeyUp = 0x0002
)

// mouseInputWrap and keybdInputWrap mirror the C INPUT union for the mouse
// and keyboard cases respectively. On amd64 Go pads the leading uint32 to
// 8-byte alignment because the payload contains a uintptr, which happens to
// reproduce the real C layout (type:4 + pad:4 + payload) - this is the same
// trick used by most minimal Win32-from-Go SendInput implementations.
type mouseInputWrap struct {
	Type uint32
	Mi   struct {
		Dx, Dy      int32
		MouseData   uint32
		DwFlags     uint32
		Time        uint32
		DwExtraInfo uintptr
	}
}

type keybdInputWrap struct {
	Type uint32
	Ki   struct {
		Vk          uint16
		Scan        uint16
		DwFlags     uint32
		Time        uint32
		DwExtraInfo uintptr
	}
}

func screenSize() (int32, int32) {
	cx, _, _ := procGetSystemMetrics.Call(uintptr(smCxScreen))
	cy, _, _ := procGetSystemMetrics.Call(uintptr(smCyScreen))
	return int32(cx), int32(cy)
}

func sendMouse(dx, dy int32, flags uint32) {
	in := mouseInputWrap{Type: inputMouse}
	in.Mi.Dx, in.Mi.Dy = dx, dy
	in.Mi.DwFlags = flags
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func sendKey(vk uint16, up bool) {
	in := keybdInputWrap{Type: inputKeyboard}
	in.Ki.Vk = vk
	if up {
		in.Ki.DwFlags = keyEventKeyUp
	}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

// keyToVK maps the small set of JS KeyboardEvent.key values we care about to
// Windows virtual-key codes. Anything else falls back to VkKeyScanW for
// single printable characters.
func keyToVK(key string) (uint16, bool) {
	switch key {
	case "Enter":
		return 0x0D, true
	case "Backspace":
		return 0x08, true
	case "Tab":
		return 0x09, true
	case "Escape":
		return 0x1B, true
	case "ArrowLeft":
		return 0x25, true
	case "ArrowUp":
		return 0x26, true
	case "ArrowRight":
		return 0x27, true
	case "ArrowDown":
		return 0x28, true
	case "Delete":
		return 0x2E, true
	case "Shift":
		return 0x10, true
	case "Control":
		return 0x11, true
	case "Alt":
		return 0x12, true
	case " ":
		return 0x20, true
	}
	if len([]rune(key)) == 1 {
		r, _, _ := procVkKeyScanW.Call(uintptr([]rune(key)[0]))
		if int16(r) != -1 {
			return uint16(r) & 0xFF, true
		}
	}
	return 0, false
}

// applyInput translates one viewer-side input event (normalized 0..1 mouse
// coordinates, or a key name) into a real SendInput call on this machine.
func applyInput(ev inputEvent) {
	switch ev.T {
	case "move":
		w, h := screenSize()
		sendMouse(int32(ev.X*65535), int32(ev.Y*65535), mouseEventMove|mouseEventAbsolute)
		_ = w
		_ = h
	case "down":
		flag := map[int]uint32{0: mouseEventLeftDown, 1: mouseEventMiddleDown, 2: mouseEventRightDown}[ev.Button]
		if flag != 0 {
			sendMouse(0, 0, flag)
		}
	case "up":
		flag := map[int]uint32{0: mouseEventLeftUp, 1: mouseEventMiddleUp, 2: mouseEventRightUp}[ev.Button]
		if flag != 0 {
			sendMouse(0, 0, flag)
		}
	case "scroll":
		in := mouseInputWrap{Type: inputMouse}
		in.Mi.DwFlags = mouseEventWheel
		in.Mi.MouseData = uint32(int32(-ev.DY))
		procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	case "keydown":
		if vk, ok := keyToVK(ev.Key); ok {
			sendKey(vk, false)
		}
	case "keyup":
		if vk, ok := keyToVK(ev.Key); ok {
			sendKey(vk, true)
		}
	}
}

// ---- mandatory on-screen indicator -----------------------------------
//
// This banner is shown for the full duration of every remote session and is
// owned entirely by this file: runRemoteSession() cannot start streaming
// without calling indicatorShow() first, and there is no parameter that
// suppresses it. That's intentional - unattended access on an enrolled
// device should still be visible to whoever is sitting at it.

var (
	indMu   sync.Mutex
	indHwnd syscall.Handle
	indDone chan struct{}
)

func indicatorShow() error {
	indMu.Lock()
	if indHwnd != 0 {
		indMu.Unlock()
		return nil // already showing
	}
	ready := make(chan error, 1)
	indDone = make(chan struct{})
	indMu.Unlock()
	go runIndicatorWindow(ready, indDone)
	return <-ready
}

func indicatorHide() {
	indMu.Lock()
	hwnd := indHwnd
	done := indDone
	indMu.Unlock()
	if hwnd == 0 {
		return
	}
	postMessage(hwnd, wmClose, 0, 0)
	if done != nil {
		<-done
	}
}

const (
	wsPopup        = 0x80000000
	wsVisible      = 0x10000000
	wsExTopmost    = 0x00000008
	wsExToolWindow = 0x00000080
	swShow         = 5
	wmDestroy      = 0x0002
	wmClose        = 0x0010
	wmPaint        = 0x000F
)

var (
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procShowWindow       = user32.NewProc("ShowWindow")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procBeginPaint       = user32.NewProc("BeginPaint")
	procEndPaint         = user32.NewProc("EndPaint")
	procFillRect         = user32.NewProc("FillRect")
	procDrawTextW        = user32.NewProc("DrawTextW")
	procSetTextColor     = user32.NewProc("SetTextColor")
	procSetBkMode        = user32.NewProc("SetBkMode")

	gdi32                = syscall.NewLazyDLL("gdi32.dll")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")

	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

type rect struct{ Left, Top, Right, Bottom int32 }
type point struct{ X, Y int32 }
type msg struct {
	Hwnd    syscall.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}
type paintStruct struct {
	Hdc         syscall.Handle
	FErase      int32
	RcPaint     rect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}
type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     syscall.Handle
	HIcon         syscall.Handle
	HCursor       syscall.Handle
	HbrBackground syscall.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       syscall.Handle
}

func postMessage(hwnd syscall.Handle, m uint32, w, l uintptr) {
	procPostMessageW.Call(uintptr(hwnd), uintptr(m), w, l)
}

const bannerText = "Remote support session active"

// m is uintptr, not uint32: syscall.NewCallback requires every parameter of
// a callback function to be exactly pointer-sized (8 bytes on amd64). A
// uint32 here silently breaks the calling convention and crashes the
// process at the OS level instead of producing a catchable Go panic.
func indicatorWndProc(hwnd syscall.Handle, m uintptr, wParam, lParam uintptr) uintptr {
	switch m {
	case wmPaint:
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
		brush, _, _ := procCreateSolidBrush.Call(0x000033CC) // BGR: strong red
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&ps.RcPaint)), brush)
		procSetBkMode.Call(hdc, 1) // TRANSPARENT
		procSetTextColor.Call(hdc, 0x00FFFFFF)
		text, _ := syscall.UTF16PtrFromString(bannerText)
		r := ps.RcPaint
		const dtCenter, dtVCenter, dtSingleLine = 0x0001, 0x0004, 0x0020
		procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(text)), ^uintptr(0),
			uintptr(unsafe.Pointer(&r)), dtCenter|dtVCenter|dtSingleLine)
		procEndPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(m), wParam, lParam)
	return ret
}

// runIndicatorWindow must run on a dedicated OS thread for the lifetime of
// the window (standard Win32 requirement: a window's messages must be
// pumped from the thread that created it).
func runIndicatorWindow(ready chan<- error, done chan<- struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(done)
	defer func() {
		if r := recover(); r != nil {
			logCrash(r)
			select {
			case ready <- fmt.Errorf("panic: %v", r):
			default:
			}
		}
	}()

	className, _ := syscall.UTF16PtrFromString("IQSoftRemoteIndicator")
	hInstance, _, _ := procGetModuleHandleW.Call(0)

	wc := wndClassEx{
		CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		LpfnWndProc:   syscall.NewCallback(indicatorWndProc),
		HInstance:     syscall.Handle(hInstance),
		LpszClassName: className,
	}
	if a, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); a == 0 {
		ready <- fmt.Errorf("RegisterClassEx failed")
		return
	}

	sw, _, _ := procGetSystemMetrics.Call(uintptr(smCxScreen))
	screenW := int32(sw)
	const width, height = 420, 30
	x := (screenW - width) / 2

	hwnd, _, _ := procCreateWindowExW.Call(
		uintptr(wsExTopmost|wsExToolWindow),
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(className)),
		uintptr(wsPopup|wsVisible),
		uintptr(x), 0, width, height,
		0, 0, hInstance, 0,
	)
	if hwnd == 0 {
		ready <- fmt.Errorf("CreateWindowEx failed")
		return
	}
	indMu.Lock()
	indHwnd = syscall.Handle(hwnd)
	indMu.Unlock()
	procShowWindow.Call(hwnd, swShow)
	ready <- nil

	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	procDestroyWindow.Call(hwnd)
	indMu.Lock()
	indHwnd = 0
	indMu.Unlock()
}