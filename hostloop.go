// Copyright 2025 The GoGPU Authors
// SPDX-License-Identifier: MIT

// PROTOTYPE (dvoyni/cog#617, #618): a loop the host owns. Throwaway; the shape
// is what is being judged, not the code.

package gogpu

import (
	"sync/atomic"
)

// HostLoop lets a host that owns its own loop drive an App one step at a
// time, instead of handing the thread to App.Run.
//
// Every call must run on the thread that called Start, which must be the OS
// main thread, locked. gogpu runs no render thread and no Windows modal
// timer: during a move or resize the frame freezes until the user lets go.
type HostLoop struct {
	app       *App
	framePool uintptr // macOS autorelease pool open between BeginFrame and EndFrame
}

// NewHostLoop returns a loop over app. Callbacks and windows stay on app.
func NewHostLoop(app *App) *HostLoop {
	return &HostLoop{app: app}
}

// Start creates the window, device and surface and returns without blocking.
func (loop *HostLoop) Start() error {
	a := loop.app
	if a.hostLoopStarted {
		return ErrHostLoopStarted
	}
	platWindow, err := a.initPlatform()
	if err != nil {
		return err
	}
	// No modal tick in the host-owned path (cog#616).
	platWindow.SetModalFrameCallback(nil)
	a.renderLoop = &inlineRenderLoop{}

	if err := a.initRenderer(platWindow); err != nil {
		platWindow.Destroy()
		a.manager.Destroy()
		return err
	}

	a.hostLoopStarted = true
	a.startedWindow = platWindow
	platWindow.Show()
	a.syncInitialSurfaceSize()
	a.registerPrimaryWindow(platWindow)
	a.startRunLoop()
	return nil
}

// PumpEvents clears last pump's input edges, drains and dispatches every
// pending platform event, records the latest resize and ends the gesture
// frame. It never waits, updates or draws.
func (loop *HostLoop) PumpEvents() {
	a := loop.app
	if a.inputState != nil {
		a.inputState.Update()
	}
	a.processEventsMultiThread()
}

// Running reports whether the app is still running: false once the window
// was closed or Quit was called.
func (loop *HostLoop) Running() bool {
	a := loop.app
	return a.running.Load() && (a.platWindow == nil || !a.platWindow.ShouldClose())
}

// BeginFrame applies the latest window size to the surface and acquires the
// next swapchain image. It returns false, without blocking, when there is no
// frame to draw: the window is hidden, minimized or zero-sized, the compositor
// frame gate is closed, or acquire failed. The Context is valid until EndFrame.
func (loop *HostLoop) BeginFrame(window *Window) (*Context, bool) {
	a := loop.app
	if window == nil || !window.visible || window.surface == nil {
		return nil, false
	}
	ws := window.surface
	if window == a.primaryWindow {
		if w, h, ok := a.renderLoop.ConsumePendingResize(); ok {
			a.renderer.Resize(int(w), int(h))
		}
	}
	physW, physH := window.platWindow.PhysicalSize()
	if physW <= 0 || physH <= 0 {
		// Minimized or hidden: unconfigure and draw nothing, never acquire (cog#616).
		a.renderer.ResizeSurface(ws, 0, 0)
		return nil, false
	}
	if !ws.CanRender() && ws.surface != nil {
		// Unconfigured or lost: force the reconfigure even at an unchanged
		// size, which resize would otherwise skip as a no-op.
		ws.width = 0
		ws.height = 0
	}
	a.syncFrameSurfaceSize(ws, physW, physH)
	if !a.frameCallbackReady() {
		return nil, false
	}

	loop.framePool = pushFramePool()
	a.renderer.DrainDeferredDestroys()
	ws.prepareLazyAcquire()
	a.renderer.currentSurface = ws
	if !ws.ensureFrameStarted() {
		a.renderer.endFrameStats(true)
		ws.resetLazyState()
		a.renderer.currentSurface = nil
		popFramePool(loop.framePool)
		loop.framePool = 0
		return nil, false
	}

	window.frameContext = Context{
		renderer:    a.renderer,
		surface:     ws,
		scaleFactor: window.platWindow.ScaleFactor(),
	}
	if window.frameContext.scaleFactor <= 0 {
		window.frameContext.scaleFactor = 1
	}
	return &window.frameContext, true
}

// EndFrame submits what the frame recorded and presents it. When the surface
// turned out outdated, the frame is dropped and a redraw requested.
func (loop *HostLoop) EndFrame(context *Context) {
	a := loop.app
	ws := context.surface
	if a.renderer.endFrameForSurface(ws) {
		a.RequestRedraw()
	}
	ws.resetLazyState()
	a.renderer.currentSurface = nil
	a.renderer.pollSubmissions()
	popFramePool(loop.framePool)
	loop.framePool = 0
}

// Stop waits for the GPU, releases the device and surface and closes the
// window. The app cannot be started again.
func (loop *HostLoop) Stop() {
	a := loop.app
	a.running.Store(false)
	a.shutdown(a.startedWindow)
	a.lifecycle = AppIdle
}

// inlineRenderLoop runs render-thread work on the calling thread: the
// host-owned path has no render thread.
type inlineRenderLoop struct {
	pendingWidth  atomic.Uint32
	pendingHeight atomic.Uint32
	resizePending atomic.Bool
}

func (loop *inlineRenderLoop) RunOnRenderThreadVoid(fn func()) { fn() }

func (loop *inlineRenderLoop) Stop() {}

func (loop *inlineRenderLoop) RequestResize(width, height uint32) {
	loop.pendingWidth.Store(width)
	loop.pendingHeight.Store(height)
	loop.resizePending.Store(true)
}

func (loop *inlineRenderLoop) ConsumePendingResize() (uint32, uint32, bool) {
	if !loop.resizePending.Swap(false) {
		return 0, 0, false
	}
	return loop.pendingWidth.Load(), loop.pendingHeight.Load(), true
}
