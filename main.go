package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"
	"github.com/spf13/cobra"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
	"golang.org/x/sys/unix"
)

const (
	DepthWithAlpha = 32
	ClassTrueColor = 4
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func MatchVisualInfo(depthInfos []xproto.DepthInfo, depth byte, class byte) *xproto.VisualInfo {
	for _, depthInfo := range depthInfos {
		if depthInfo.Depth != depth {
			continue
		}

		for _, visual := range depthInfo.Visuals {
			if visual.Class == class {
				return &visual
			}
		}
	}

	return nil
}

type ImageWindow struct {
	// X resources
	conn          *xgb.Conn
	screen        *xproto.ScreenInfo
	windowID      xproto.Window
	transparentGc xproto.Gcontext
	imageGc       xproto.Gcontext

	// the image we want to render
	image image.Image

	// bookkeeping for debounced rendering
	imageOpacity   float64
	windowWidth    int
	windowHeight   int
	nextRedraw     time.Time
	dirty          bool
	renderMu       sync.Mutex
	wg             sync.WaitGroup
	cancelRenderer context.CancelFunc
}

func (imageWindow *ImageWindow) setupX() error {
	conn, err := xgb.NewConn()
	if err != nil {
		return fmt.Errorf("new conn: %w", err)
	}

	imageWindow.conn = conn

	setup := xproto.Setup(conn)
	screen := setup.DefaultScreen(conn)
	imageWindow.screen = screen

	err = shm.Init(conn)
	if err != nil {
		return fmt.Errorf("init shm: %w", err)
	}

	return nil
}

func (imageWindow *ImageWindow) loadImage(imageBytes []byte) error {
	img, _, err := image.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		return fmt.Errorf("decode image: %w", err)
	}

	imageWindow.image = img
	imageWindow.windowWidth = img.Bounds().Dx()
	imageWindow.windowHeight = img.Bounds().Dy()

	return nil
}

func NewImageWindow(
	initialOpacity float64,
	imageBytes []byte,
) (*ImageWindow, error) {
	imageWindow := &ImageWindow{
		imageOpacity: initialOpacity,
	}

	err := imageWindow.loadImage(imageBytes)
	if err != nil {
		return nil, fmt.Errorf("load image: %w", err)
	}

	err = imageWindow.setupX()
	if err != nil {
		return nil, fmt.Errorf("setup x: %w", err)
	}

	rendererCtx, cancel := context.WithCancel(context.Background())
	imageWindow.cancelRenderer = cancel

	go imageWindow.startRenderer(rendererCtx)

	return imageWindow, nil
}

func (display *ImageWindow) requestRedraw() {
	display.renderMu.Lock()
	display.dirty = true
	display.nextRedraw = time.Now().Add(50 * time.Millisecond)
	display.renderMu.Unlock()
}

func (display *ImageWindow) startRenderer(ctx context.Context) {
	display.wg.Add(1)
	defer display.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			time.Sleep(5 * time.Millisecond)
		}

		display.renderMu.Lock()
		dirty := display.dirty
		nextRedraw := display.nextRedraw
		display.renderMu.Unlock()

		if dirty && time.Now().After(nextRedraw) {
			display.renderMu.Lock()
			display.dirty = false
			display.renderMu.Unlock()

			err := display.RenderImage()
			if err != nil {
				fmt.Println("render image:", err)
			}

		}
	}
}

func (display *ImageWindow) Close() {
	display.cancelRenderer()
	display.conn.Close()
	display.wg.Wait()
}

func (display *ImageWindow) CreateWindow() error {
	visualInfo := MatchVisualInfo(display.screen.AllowedDepths, DepthWithAlpha, ClassTrueColor)
	if visualInfo == nil {
		return fmt.Errorf("no visual with required parameters found")
	}

	colorMapID, err := xproto.NewColormapId(display.conn)
	if err != nil {
		return fmt.Errorf("new colormap id: %w", err)
	}

	windowID, err := xproto.NewWindowId(display.conn)
	if err != nil {
		return fmt.Errorf("new window id: %w", err)
	}

	display.windowID = windowID

	err = xproto.CreateColormapChecked(
		display.conn,
		xproto.ColormapAllocNone,
		colorMapID,
		display.screen.Root,
		visualInfo.VisualId,
	).Check()
	if err != nil {
		return fmt.Errorf("create colormap: %w", err)
	}

	mask := uint32(xproto.CwColormap | xproto.CwBorderPixel | xproto.CwBackPixel)
	values := []uint32{
		0, // black bg
		0, // black border
		uint32(colorMapID),
	}

	imageWidth := display.image.Bounds().Dx()
	imageHeight := display.image.Bounds().Dy()

	err = xproto.CreateWindowChecked(
		display.conn,
		DepthWithAlpha,
		windowID,
		display.screen.Root,           // parent
		0,                             // x
		0,                             // y
		uint16(imageWidth),            // width
		uint16(imageHeight),           // height
		0,                             // border width
		xproto.WindowClassInputOutput, // class
		visualInfo.VisualId,
		mask,
		values,
	).Check()
	if err != nil {
		return fmt.Errorf("create window: %w", err)
	}

	display.windowWidth = imageWidth
	display.windowHeight = imageHeight

	// This call to ChangeWindowAttributes could be factored out and
	// included with the above CreateWindow call, but it is left here for
	// instructive purposes. It tells X to send us events when the 'structure'
	// of the window is changed (i.e., when it is resized, mapped, unmapped,
	// etc.) and when a key press or a key release has been made when the
	// window has focus.
	// We also set the 'BackPixel' to white so that the window isn't butt ugly.
	xproto.ChangeWindowAttributes(display.conn, display.windowID,
		xproto.CwBackPixel|xproto.CwEventMask,
		[]uint32{
			0x00000000,
			xproto.EventMaskStructureNotify |
				xproto.EventMaskExposure |
				xproto.EventMaskButtonPress,
		})

	err = xproto.MapWindowChecked(display.conn, windowID).Check()
	if err != nil {
		return fmt.Errorf("map window :%w", err)
	}

	err = display.setClass()
	if err != nil {
		return fmt.Errorf("set class: %w", err)
	}

	imageGc, err := xproto.NewGcontextId(display.conn)
	if err != nil {
		return fmt.Errorf("new graphics context id: %w", err)
	}

	err = xproto.CreateGCChecked(
		display.conn,
		imageGc,
		xproto.Drawable(display.windowID),
		0,
		[]uint32{},
	).Check()
	if err != nil {
		return fmt.Errorf("create graphics context: %w", err)
	}

	display.imageGc = imageGc

	return nil
}

func (display *ImageWindow) RenderImage() error {
	geom, err := xproto.GetGeometry(display.conn, xproto.Drawable(display.windowID)).Reply()
	if err != nil {
		return fmt.Errorf("get geometry: %w", err)
	}

	originalBounds := display.image.Bounds()
	aspect := float64(originalBounds.Dx()) / float64(originalBounds.Dy())

	width := int(geom.Width)
	height := int(geom.Height)

	xOffset := 0
	yOffset := 0

	newAspect := float64(width) / float64(height)

	if newAspect > aspect {
		newWidth := int(aspect * float64(height))
		xOffset = (width - newWidth) / 2
		width = newWidth
	} else {
		newHeight := int(float64(width) / aspect)
		yOffset = (height - newHeight) / 2
		height = newHeight
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))

	const fullAlpha = 255
	alpha := uint8(fullAlpha * display.imageOpacity)
	mask := image.NewUniform(color.Alpha{alpha})

	draw.NearestNeighbor.Scale(
		img,
		img.Bounds(),
		display.image,
		display.image.Bounds(),
		draw.Over,
		&draw.Options{
			SrcMask: mask,
		},
	)

	data := make([]byte, 0, width*height*4)

	for y := 0; y < height; y += 1 {
		for x := 0; x < width; x += 1 {
			r, g, b, a := img.At(x, y).RGBA()
			// xorg is bgr
			data = append(data, byte(b))
			data = append(data, byte(g))
			data = append(data, byte(r))
			data = append(data, byte(a))
		}
	}

	size := len(data)

	shmID, err := unix.SysvShmGet(unix.IPC_PRIVATE, size, unix.IPC_CREAT|unix.IPC_EXCL|0o600)
	if err != nil {
		return fmt.Errorf("create shared memory segment: %w", err)
	}
	defer func() {
		// it is important to remove the shared memory segment because it
		// persists even if the process is destroyed.
		_, err := unix.SysvShmCtl(shmID, unix.IPC_RMID, nil)
		if err != nil {
			fmt.Println("destroy shared memmory segment:", err)
		}
	}()

	buf, err := unix.SysvShmAttach(shmID, 0, 0)
	if err != nil {
		return fmt.Errorf("attach to shared memory segment: %w", err)
	}

	defer func() {
		err := unix.SysvShmDetach(buf)
		if err != nil {
			fmt.Println("detach from shared memory segment:", err)
		}
	}()

	n := copy(buf, data)
	if n != size {
		return fmt.Errorf("copy failed, want %d bytes, got %d", size, n)
	}

	segID, err := shm.NewSegId(display.conn)
	if err != nil {
		return fmt.Errorf("new segment id: %w", err)
	}

	err = shm.AttachChecked(display.conn, segID, uint32(shmID), false).Check()
	if err != nil {
		return fmt.Errorf("attach to shared memory segment (X): %w", err)
	}

	defer func() {
		err = shm.DetachChecked(display.conn, segID).Check()
		if err != nil {
			fmt.Println("detach from shared memory (X):", err)
		}
	}()

	err = shm.PutImageChecked(
		display.conn,
		xproto.Drawable(display.windowID),
		display.imageGc,
		uint16(width),
		uint16(height),
		0, // src x
		0, // src y
		uint16(width),
		uint16(height),
		int16(xOffset), // dst x
		int16(yOffset), // dst y
		DepthWithAlpha, // depth
		xproto.ImageFormatZPixmap,
		0,
		segID,
		0,
	).Check()
	if err != nil {
		return fmt.Errorf("put image: %w", err)
	}

	return nil
}

func (display *ImageWindow) setClass() error {
	class := "overlay\x00overlay\x00"

	const format8Bit = 8

	err := xproto.ChangePropertyChecked(
		display.conn,
		xproto.PropModeReplace,
		display.windowID,
		xproto.AtomWmClass,
		xproto.AtomString,
		format8Bit,
		uint32(len(class)),
		[]byte(class),
	).Check()
	if err != nil {
		return fmt.Errorf("set class: %w", err)
	}

	return nil
}

func (display *ImageWindow) HandleEvents() error {
	for {
		ev, xerr := display.conn.WaitForEvent()
		if ev == nil && xerr == nil {
			return fmt.Errorf("got no event but err is nil, exiting")
		}

		switch event := ev.(type) {
		case xproto.ConfigureNotifyEvent:
			if display.windowWidth != int(event.Width) || display.windowHeight != int(event.Height) {
				display.windowWidth = int(event.Width)
				display.windowHeight = int(event.Height)
				display.requestRedraw()
			}
		case xproto.ButtonPressEvent:
			x := min(display.windowWidth, max(0, int(event.EventX)))
			display.imageOpacity = float64(x) / float64(display.windowWidth)
			display.requestRedraw()
		case xproto.DestroyNotifyEvent:
			return nil
		}
	}
}

func run() error {
	initialOpacity := 0.0

	cmd := &cobra.Command{
		Use:           "xoverlay <file>",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			filename := args[0]

			var imageBytes []byte
			var err error
			if filename == "-" {
				imageBytes, err = io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read image bytes from stdin: %w", err)
				}
			} else {
				imageBytes, err = os.ReadFile(filename)
				if err != nil {
					return fmt.Errorf("read image bytes from file: %w", err)
				}
			}

			initialOpacity = min(1.0, max(0.0, initialOpacity))

			display, err := NewImageWindow(initialOpacity, imageBytes)
			if err != nil {
				return fmt.Errorf("new display: %w", err)
			}
			defer display.Close()

			err = display.CreateWindow()
			if err != nil {
				return fmt.Errorf("create window: %w", err)
			}

			// initial draw
			display.requestRedraw()

			err = display.HandleEvents()
			if err != nil {
				return fmt.Errorf("handle events: %w", err)
			}

			return nil
		},
	}

	flags := cmd.Flags()

	const defaultInitialOpacity = 0.5

	flags.Float64Var(&initialOpacity, "opacity", defaultInitialOpacity, "set the initial opacity")

	err := cmd.Execute()
	if err != nil {
		return fmt.Errorf("run command: %w", err)
	}

	return nil
}
