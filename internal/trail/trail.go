package trail

import (
	"log/slog"
	"time"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/shape"
	"github.com/BurntSushi/xgb/xfixes"
	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/xwindow"
)

const (
	Duration    = 2 * time.Second // 軌跡の表示時間
	MinDistance = 1               // 軌跡を追加する最小移動距離（ピクセル）
	LineWidth   = 8               // 軌跡の線の太さ
	Color       = 0xFF0000        // 軌跡の色（赤）
	atomOpacity = "_NET_WM_WINDOW_OPACITY"
)

// Segment 軌跡の線分
type Segment struct {
	x1, y1    int
	x2, y2    int
	timestamp time.Time
	window    *xwindow.Window
	gc        xproto.Gcontext
}

// Manager 軌跡管理
type Manager struct {
	xConn  *xgb.Conn
	xuConn *xgbutil.XUtil
	trails []*Segment
	lastX  int
	lastY  int
}

// NewManager 軌跡マネージャを作成
func NewManager(xConn *xgb.Conn, xuConn *xgbutil.XUtil) *Manager {
	return &Manager{
		xConn:  xConn,
		xuConn: xuConn,
		lastX:  -1,
		lastY:  -1,
	}
}

// ShouldAdd 軌跡を追加すべきか判定
func (m *Manager) ShouldAdd(x, y int) bool {
	if m.lastX == -1 || m.lastY == -1 {
		return false
	}
	dx := x - m.lastX
	dy := y - m.lastY
	distance := dx*dx + dy*dy
	return distance >= MinDistance*MinDistance
}

// Add 軌跡セグメントを追加
func (m *Manager) Add(x1, y1, x2, y2 int) {
	if m.xConn == nil || m.xuConn == nil {
		return
	}

	// 線分を囲む矩形を計算
	minX := min(x1, x2) - 5
	minY := min(y1, y2) - 5
	maxX := max(x1, x2) + 5
	maxY := max(y1, y2) + 5
	width := maxX - minX
	height := maxY - minY

	// ウィンドウを生成
	win, err := xwindow.Generate(m.xuConn)
	if err != nil {
		slog.Error("軌跡ウィンドウ生成エラー", "error", err)
		return
	}

	if err := win.CreateChecked(
		m.xuConn.RootWin(),
		minX, minY,
		width, height,
		xproto.CwBackPixel|xproto.CwOverrideRedirect,
		0xff0000, // 黒背景（SHAPEマスクで隠される）
		1,
	); err != nil {
		slog.Error("軌跡ウィンドウ作成エラー", "error", err)
		return
	}

	// GCを作成
	gc, _ := xproto.NewGcontextId(m.xConn)
	if err := xproto.CreateGCChecked(
		m.xConn,
		gc,
		xproto.Drawable(win.Id),
		xproto.GcForeground|xproto.GcLineWidth|xproto.GcCapStyle|xproto.GcJoinStyle,
		[]uint32{Color, LineWidth, xproto.CapStyleRound, xproto.JoinStyleRound},
	).Check(); err != nil {
		slog.Error("GC作成エラー", "error", err)
		return
	}

	// ウィンドウに直接線を描画
	xproto.PolyLine(
		m.xConn,
		xproto.CoordModeOrigin,
		xproto.Drawable(win.Id),
		gc,
		[]xproto.Point{
			{X: int16(x1 - minX), Y: int16(y1 - minY)},
			{X: int16(x2 - minX), Y: int16(y2 - minY)},
		},
	)

	// マスクを作成して線の部分だけを表示
	setup := xproto.Setup(m.xConn)
	screen := setup.DefaultScreen(m.xConn)
	maskPixmap, _ := xproto.NewPixmapId(m.xConn)
	if err := xproto.CreatePixmapChecked(
		m.xConn,
		1, // 1-bit depth
		maskPixmap,
		xproto.Drawable(screen.Root),
		uint16(width),
		uint16(height),
	).Check(); err != nil {
		slog.Error("マスクPixmap作成エラー", "error", err)
		return
	}

	maskGC, _ := xproto.NewGcontextId(m.xConn)
	if err := xproto.CreateGCChecked(
		m.xConn,
		maskGC,
		xproto.Drawable(maskPixmap),
		xproto.GcForeground|xproto.GcBackground,
		[]uint32{0, 0},
	).Check(); err != nil {
		slog.Error("マスクGC作成エラー", "error", err)
		return
	}

	// マスクをクリア（透明）
	xproto.PolyFillRectangle(
		m.xConn,
		xproto.Drawable(maskPixmap),
		maskGC,
		[]xproto.Rectangle{{X: 0, Y: 0, Width: uint16(width), Height: uint16(height)}},
	)

	// マスクに線を描画（不透明）
	xproto.ChangeGC(m.xConn, maskGC, xproto.GcForeground|xproto.GcLineWidth|xproto.GcCapStyle|xproto.GcJoinStyle,
		[]uint32{1, LineWidth, xproto.CapStyleRound, xproto.JoinStyleRound})
	xproto.PolyLine(
		m.xConn,
		xproto.CoordModeOrigin,
		xproto.Drawable(maskPixmap),
		maskGC,
		[]xproto.Point{
			{X: int16(x1 - minX), Y: int16(y1 - minY)},
			{X: int16(x2 - minX), Y: int16(y2 - minY)},
		},
	)

	// マスクを適用
	if err := shape.Init(m.xConn); err != nil {
		slog.Error("Shape拡張初期化エラー", "error", err)
		return
	}
	shape.Mask(
		m.xConn,
		shape.SoSet,
		shape.SkBounding,
		xproto.Window(win.Id),
		0, 0,
		maskPixmap,
	)

	xproto.FreeGC(m.xConn, maskGC)
	xproto.FreePixmap(m.xConn, maskPixmap)

	m.setupWindowClickThrough(win)
	m.xConn.Sync()

	// クリックスルー設定後にウィンドウを表示
	win.Map()

	segment := &Segment{
		x1: x1, y1: y1, x2: x2, y2: y2,
		timestamp: time.Now(),
		window:    win,
		gc:        gc,
	}

	m.trails = append(m.trails, segment)
}

// setupWindowClickThrough 単一ウィンドウのクリックスルーを設定
func (m *Manager) setupWindowClickThrough(win *xwindow.Window) {
	region, err := xfixes.NewRegionId(m.xConn)
	if err != nil {
		slog.Error("軌跡クリックスルー設定エラー(NewRegionId)", "error", err)
		return
	}
	defer xfixes.DestroyRegion(m.xConn, region)

	if err := xfixes.CreateRegionChecked(m.xConn, region, []xproto.Rectangle{{}}).Check(); err != nil {
		slog.Error("軌跡クリックスルー設定エラー(CreateRegion)", "error", err)
		return
	}

	winID := xproto.Window(win.Id)
	if err := xfixes.SetWindowShapeRegionChecked(m.xConn, winID, shape.SkInput, 0, 0, region).Check(); err != nil {
		slog.Error("軌跡クリックスルー設定エラー(SetWindowShapeRegion)", "error", err)
		return
	}
}

// Update 軌跡の透明度を更新し、古い軌跡を削除
func (m *Manager) Update() {
	now := time.Now()

	newTrails := make([]*Segment, 0, len(m.trails))

	for _, segment := range m.trails {
		elapsed := now.Sub(segment.timestamp)

		if elapsed > Duration {
			// GCを解放
			xproto.FreeGC(m.xConn, segment.gc)
			// ウィンドウをアンマップしてから破棄
			segment.window.Unmap()
			segment.window.Destroy()
			continue
		}

		newTrails = append(newTrails, segment)
	}

	m.trails = newTrails
	m.xConn.Sync()
}

// UpdatePosition 最後の位置を更新
func (m *Manager) UpdatePosition(x, y int) {
	m.lastX = x
	m.lastY = y
}

// GetLastPosition 最後の位置を取得
func (m *Manager) GetLastPosition() (int, int) {
	return m.lastX, m.lastY
}

// Clear すべての軌跡をクリア
func (m *Manager) Clear() {
	for _, segment := range m.trails {
		if segment.window != nil {
			xproto.FreeGC(m.xConn, segment.gc)
			segment.window.Unmap()
			segment.window.Destroy()
		}
	}
	m.trails = nil
	m.lastX = -1
	m.lastY = -1
}
