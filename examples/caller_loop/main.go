// PROTOTYPE (dvoyni/cog#617, #618): a window and a cleared frame driven from
// our own for loop through gogpu.HostLoop, with no App.Run and no gogpu render
// thread. Throwaway.
//
//	go run ./examples/caller_loop -api vulkan
//	go run ./examples/caller_loop -api dx12
//	go run ./examples/caller_loop -api metal
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"time"

	"github.com/gogpu/gogpu"
	"github.com/gogpu/gogpu/gpu/types"
)

func init() { runtime.LockOSThread() } // as the kernel does

func main() {
	apiName := flag.String("api", "auto", "auto, vulkan, dx12, metal, gles")
	vsync := flag.Bool("vsync", true, "Fifo present")
	seconds := flag.Int("seconds", 0, "quit after this many seconds; 0 runs until the window closes")
	memoryProfile := flag.String("memprofile", "", "write an allocation profile of every allocation here")
	flag.Parse()
	if *memoryProfile != "" {
		runtime.MemProfileRate = 1
	}
	api := map[string]types.GraphicsAPI{
		"auto": types.GraphicsAPIAuto, "vulkan": types.GraphicsAPIVulkan,
		"dx12": types.GraphicsAPIDX12, "metal": types.GraphicsAPIMetal, "gles": types.GraphicsAPIGLES,
	}[*apiName]

	app := gogpu.NewApp(gogpu.DefaultConfig().
		WithTitle("caller loop").
		WithSize(800, 600).
		WithGraphicsAPI(api).
		WithVSync(*vsync))
	app.OnResize(func(width, height int) { fmt.Printf("resize %dx%d\n", width, height) })
	loop := gogpu.NewHostLoop(app)
	if err := loop.Start(); err != nil {
		log.Fatal(err)
	}
	if err := app.Run(); err != gogpu.ErrHostLoopStarted {
		log.Fatalf("Run while a HostLoop has started: got %v", err)
	}
	window := app.PrimaryWindow()

	allocations := []metrics.Sample{{Name: "/gc/heap/allocs:objects"}}
	allocationCount := func() uint64 {
		metrics.Read(allocations)
		return allocations[0].Value.Uint64()
	}

	start := time.Now()
	lastReport := start
	var pumps, frames, skipped int
	var pumpAllocations, frameAllocations uint64
	var longestPump, longestBegin, longestEnd time.Duration
	var backend string
	var width, height int
	for loop.Running() {
		pumpAt := time.Now()
		before := allocationCount()
		loop.PumpEvents()
		pumpAllocations += allocationCount() - before
		longestPump = max(longestPump, time.Since(pumpAt))
		pumps++

		beginAt := time.Now()
		before = allocationCount()
		context, ok := loop.BeginFrame(window)
		longestBegin = max(longestBegin, time.Since(beginAt))
		if !ok {
			skipped++
			time.Sleep(time.Millisecond)
		} else {
			backend = context.Backend()
			width, height = context.FramebufferSize()
			phase := time.Since(start).Seconds()
			context.Clear(float32(0.5+0.5*math.Sin(phase)), 0.2, float32(0.5+0.5*math.Cos(phase)), 1)
			endAt := time.Now()
			loop.EndFrame(context)
			longestEnd = max(longestEnd, time.Since(endAt))
			frameAllocations += allocationCount() - before
			frames++
		}

		if *seconds > 0 && time.Since(start) >= time.Duration(*seconds)*time.Second {
			app.Quit()
		}
		if time.Since(lastReport) >= time.Second {
			fmt.Printf("backend=%s pumps=%d frames=%d skipped=%d allocsPerPump=%.2f allocsPerFrame=%.2f longestPump=%v longestBegin=%v longestEnd=%v framebuffer=%dx%d\n",
				backend, pumps, frames, skipped,
				float64(pumpAllocations)/float64(max(pumps, 1)), float64(frameAllocations)/float64(max(frames, 1)),
				longestPump, longestBegin, longestEnd, width, height)
			pumps, frames, skipped = 0, 0, 0
			pumpAllocations, frameAllocations = 0, 0
			longestPump, longestBegin, longestEnd = 0, 0, 0
			lastReport = time.Now()
		}
	}
	loop.Stop()
	if *memoryProfile != "" {
		file, err := os.Create(*memoryProfile)
		if err == nil {
			runtime.GC() // the profile covers allocations up to the last GC
			_ = pprof.Lookup("allocs").WriteTo(file, 0)
			file.Close()
		}
	}
	fmt.Println("stopped cleanly")
}
