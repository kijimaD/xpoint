package ruler

import (
	"log/slog"
	"sync"
	"time"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/shape"
	"github.com/BurntSushi/xgb/xfixes"
	"github.com/BurntSushi/xgb/xinerama"
	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/keybind"
	"github.com/BurntSushi/xgbutil/xevent"
	"github.com/BurntSushi/xgbutil/xwindow"
	"github.com/kijimaD/xruler/internal/trail"
)

const (
	PollInterval    = 16 * time.Millisecond    // カーソル位置のポーリング間隔（約60fps）
	xfixesMajor     = 6                        // XFixes拡張のメジャーバージョン
	xfixesMinor     = 0                        // XFixes拡張のマイナーバージョン
	extensionXFIXES = "XFIXES"                 // XFixes拡張の名前
	atomOpacity     = "_NET_WM_WINDOW_OPACITY" // ウィンドウ不透明度を設定するアトム名
)

// Ruler X Window System上でカーソル位置を追従する水平ルーラー
type Ruler struct {
	xConn           *xgb.Conn         // X11プロトコル接続
	xuConn          *xgbutil.XUtil    // xgbutilユーティリティ接続
	windows         []*xwindow.Window // ウィンドウリスト
	screenWidth     int               // 画面の幅
	screenHeight    int               // 画面の高さ
	mode            Mode              // 動作モード
	visible         bool              // 表示状態
	trailMgr        *trail.Manager    // 軌跡管理
	mu              sync.Mutex        // ウィンドウ操作の排他制御
	toggling        bool              // トグル処理中フラグ
	xeventHealthy   bool              // xevent.Mainの健全性
	lastKeybindTest time.Time         // 最後のキーバインドテスト時刻
	errorCount      int               // 連続エラーカウント
	reinitializing  bool              // 再初期化中フラグ
}

// New ルーラーを作成
func New(mode Mode) *Ruler {
	return &Ruler{
		mode:    mode,
		visible: true,
	}
}

// Close X接続を閉じる
func (r *Ruler) Close() {
	if r.xConn != nil {
		r.xConn.Close()
	}
}

// runXEventMain xevent.Mainをpanic recoveryで実行し、クラッシュ時に再起動
func (r *Ruler) runXEventMain() {
	defer func() {
		if err := recover(); err != nil {
			slog.Error("xevent.Main panic", "error", err)
		}

		// panic時も正常終了時も、xeventを停止状態にして再起動
		r.mu.Lock()
		r.xeventHealthy = false
		r.mu.Unlock()

		slog.Warn("xevent.Main 停止検出。再起動中...")

		// 1秒待ってから再起動
		time.Sleep(1 * time.Second)

		// キーバインドを再設定
		if err := r.setupKeyboard(); err != nil {
			slog.Error("キーバインド再設定エラー", "error", err)
		}

		r.mu.Lock()
		r.xeventHealthy = true
		r.mu.Unlock()

		// 再帰的に再起動
		go r.runXEventMain()
	}()

	r.mu.Lock()
	r.xeventHealthy = true
	r.mu.Unlock()

	slog.Info("xevent.Main 起動")
	xevent.Main(r.xuConn)
	slog.Info("xevent.Main 終了（正常終了）")
}

// Run メインループ：カーソル位置を追従してウィンドウ位置を更新
func (r *Ruler) Run() {
	var lastY int = -1

	// panic recoveryを含むxevent.Mainを起動
	go r.runXEventMain()

	// 定期的にキーバインドの健全性をチェック
	keybindCheckTicker := time.NewTicker(10 * time.Second)
	defer keybindCheckTicker.Stop()

	go func() {
		for range keybindCheckTicker.C {
			r.mu.Lock()
			healthy := r.xeventHealthy
			lastTest := r.lastKeybindTest
			r.mu.Unlock()

			if !healthy {
				slog.Warn("xevent.Mainが停止しています")
			}

			// 60秒以上キーバインドが呼ばれていない場合は警告
			if time.Since(lastTest) > 60*time.Second && !lastTest.IsZero() {
				slog.Warn("キーバインドが60秒以上呼ばれていません", "last_test", lastTest.Format("15:04:05"))
			}
		}
	}()

	for {
		// panic recovery
		func() {
			defer func() {
				if err := recover(); err != nil {
					slog.Error("メインループ panic", "error", err)
				}
			}()

			// カーソル位置を取得
			cx, cy, err := r.getCursor()
			if err != nil {
				slog.Error("カーソル位置取得エラー", "error", err)

				// エラーカウントを増やす
				r.mu.Lock()
				r.errorCount++
				errCount := r.errorCount
				reinit := r.reinitializing
				r.mu.Unlock()

				// 連続10回エラーが発生したら再初期化
				if errCount >= 10 && !reinit {
					slog.Warn("連続エラー検出。X接続を再初期化します", "error_count", errCount)
					go r.reinitialize()
				}
				return
			}

			// 正常に取得できたらエラーカウントをリセット
			r.mu.Lock()
			if r.errorCount > 0 {
				r.errorCount = 0
			}
			r.mu.Unlock()

			// 位置が変わった時のみ更新（不要な描画を削減）
			if cy != lastY {
				r.mu.Lock()
				visible := r.visible
				windowsLen := len(r.windows)
				r.mu.Unlock()

				if visible && windowsLen > 0 {
					// mutex外でUpdateWindowsを実行（長時間ロックを避ける）
					r.mode.UpdateWindows(r.xConn, r.windows, cx, cy, r.screenWidth, r.screenHeight)
				}
				lastY = cy
			}

			// カーソルが移動したら軌跡を追加
			lastX, lastY := r.trailMgr.GetLastPosition()
			if cx != lastX || cy != lastY {
				r.mu.Lock()
				visible := r.visible
				r.mu.Unlock()

				if visible && lastX != -1 && lastY != -1 {
					if r.trailMgr.ShouldAdd(cx, cy) {
						r.trailMgr.Add(lastX, lastY, cx, cy)
					}
				}
				r.trailMgr.UpdatePosition(cx, cy)
			}

			r.trailMgr.Update()
		}()

		time.Sleep(PollInterval)
	}
}

// Init ルーラーの初期化：X接続の確立とウィンドウの設定
func (r *Ruler) Init() error {
	var err error

	// xgbutilユーティリティ接続を確立
	r.xuConn, err = xgbutil.NewConn()
	if err != nil {
		return err
	}

	// X11プロトコル接続を確立
	r.xConn, err = xgb.NewConn()
	if err != nil {
		return err
	}
	r.xConn.Sync()

	// 画面サイズを取得
	r.screenWidth, r.screenHeight = r.getScreenSize()

	// 軌跡マネージャを初期化
	r.trailMgr = trail.NewManager(r.xConn, r.xuConn)

	// 上下2つのウィンドウを作成
	if err := r.createWindows(); err != nil {
		return err
	}

	// クリックスルー設定（ルーラーがマウスクリックを邪魔しないようにする）
	if err := r.setupClickThrough(); err != nil {
		return err
	}

	// 透明度を設定
	if err := r.setupTransparency(); err != nil {
		return err
	}

	// キーボードイベントの設定
	if err := r.setupKeyboard(); err != nil {
		return err
	}

	return nil
}

// reinitialize X接続とウィンドウを再初期化（サスペンド復帰時など）
func (r *Ruler) reinitialize() {
	r.mu.Lock()
	if r.reinitializing {
		r.mu.Unlock()
		return
	}
	r.reinitializing = true
	wasVisible := r.visible
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.reinitializing = false
		r.errorCount = 0
		r.mu.Unlock()
	}()

	slog.Info("X接続の再初期化を開始")

	// 古い接続を閉じる
	if r.xConn != nil {
		r.xConn.Close()
	}

	// 少し待機
	time.Sleep(500 * time.Millisecond)

	// X接続を再確立
	var err error
	r.xConn, err = xgb.NewConn()
	if err != nil {
		slog.Error("X接続の再確立に失敗", "error", err)
		return
	}
	r.xConn.Sync()

	r.xuConn, err = xgbutil.NewConn()
	if err != nil {
		slog.Error("xgbutil接続の再確立に失敗", "error", err)
		return
	}

	// 画面サイズを再取得
	r.screenWidth, r.screenHeight = r.getScreenSize()

	// 軌跡マネージャを再初期化
	trailMgr := trail.NewManager(r.xConn, r.xuConn)

	// ウィンドウを再作成
	newWindows, err := r.mode.CreateWindows(r.xuConn, r.screenWidth, r.screenHeight)
	if err != nil {
		slog.Error("ウィンドウ再作成に失敗", "error", err)
		return
	}
	r.xConn.Sync()

	// クリックスルー設定（mutexロック前に実行）
	if err := r.setupClickThroughForWindows(newWindows); err != nil {
		slog.Error("クリックスルー設定に失敗", "error", err)
		return
	}

	// 透明度を設定（mutexロック前に実行）
	if err := r.setupTransparencyForWindows(newWindows); err != nil {
		slog.Error("透明度設定に失敗", "error", err)
		return
	}

	// mutexで保護して代入
	r.mu.Lock()
	r.trailMgr = trailMgr
	r.windows = newWindows
	r.mu.Unlock()

	// キーバインドを再設定
	if err := r.setupKeyboard(); err != nil {
		slog.Error("キーバインド設定に失敗", "error", err)
		return
	}

	// xevent.Mainを再起動
	go r.runXEventMain()

	// 元の表示状態に応じてウィンドウを表示/非表示
	r.mu.Lock()
	r.visible = wasVisible
	r.mu.Unlock()

	if wasVisible {
		for _, win := range r.windows {
			win.Map()
		}
		r.xConn.Sync()
	}

	slog.Info("X接続の再初期化完了")
}

// setupKeyboard キーボードイベントを設定
func (r *Ruler) setupKeyboard() error {
	// keybindを初期化
	keybind.Initialize(r.xuConn)

	// ルートウィンドウでグローバルにキーをキャプチャ
	err := keybind.KeyPressFun(
		func(X *xgbutil.XUtil, e xevent.KeyPressEvent) {
			slog.Debug("キーバインドコールバック呼び出し")
			r.mu.Lock()
			r.lastKeybindTest = time.Now()
			r.mu.Unlock()
			r.toggleVisibility()
		}).Connect(r.xuConn, r.xuConn.RootWin(), "Control-Shift-space", true)

	if err != nil {
		return err
	}

	slog.Info("キーバインド設定完了: Ctrl+Shift+Space でトグル")

	return nil
}

// toggleVisibility 表示状態を切り替え（guake方式: Map/Unmapのみ）
func (r *Ruler) toggleVisibility() {
	slog.Info("toggleVisibility 呼び出し")

	// すでに処理中なら無視
	r.mu.Lock()
	if r.toggling {
		slog.Debug("処理中のため無視")
		r.mu.Unlock()
		return
	}
	r.toggling = true
	r.mu.Unlock()

	go func() {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("toggleVisibility panic", "error", err)
			}
			r.mu.Lock()
			r.toggling = false
			r.mu.Unlock()
		}()

		slog.Debug("goroutine: mutex取得待ち...")
		r.mu.Lock()
		slog.Debug("goroutine: mutex取得完了")
		defer r.mu.Unlock()

		// 可視状態を切り替え
		r.visible = !r.visible
		slog.Info("可視状態変更", "visible", r.visible)

		if r.visible {
			slog.Debug("[1/5] 軌跡をクリア中...")
			// 軌跡をクリア
			if r.trailMgr != nil {
				r.trailMgr.Clear()
			}

			slog.Debug("[2/5] ウィンドウを表示中", "count", len(r.windows))
			// ウィンドウを表示
			for i, win := range r.windows {
				slog.Debug("ウィンドウをMap中", "index", i+1, "total", len(r.windows))
				win.Map()
			}

			slog.Debug("[3/5] カーソル位置取得中...")
			// 現在のカーソル位置でウィンドウを更新
			cx, cy, err := r.getCursor()
			if err != nil {
				slog.Error("カーソル位置取得エラー", "error", err)
			} else {
				slog.Debug("[4/5] ウィンドウ位置更新中", "cursor_x", cx, "cursor_y", cy)
				if cx != -1 && cy != -1 {
					r.mode.UpdateWindows(r.xConn, r.windows, cx, cy, r.screenWidth, r.screenHeight)
				}
			}

			slog.Debug("[5/5] Sync中...")
			r.xConn.Sync()
			slog.Info("ルーラー表示: ON")
		} else {
			slog.Debug("[1/2] ウィンドウを非表示中", "count", len(r.windows))
			// ウィンドウを非表示
			for i, win := range r.windows {
				slog.Debug("ウィンドウをUnmap中", "index", i+1, "total", len(r.windows))
				win.Unmap()
			}
			slog.Debug("[2/2] Sync中...")
			r.xConn.Sync()
			slog.Info("ルーラー表示: OFF")
		}
	}()
}

func (r *Ruler) createWindows() error {
	var err error

	r.windows, err = r.mode.CreateWindows(r.xuConn, r.screenWidth, r.screenHeight)
	if err != nil {
		return err
	}

	r.xConn.Sync()
	return nil
}

func (r *Ruler) getScreenSize() (int, int) {
	if err := xinerama.Init(r.xConn); err == nil {
		// Xineramaが有効かチェック
		if active, err := xinerama.IsActive(r.xConn).Reply(); err == nil && active.State != 0 {
			// すべてのスクリーン情報を取得
			if screens, err := xinerama.QueryScreens(r.xConn).Reply(); err == nil && len(screens.ScreenInfo) > 0 {
				// すべてのスクリーンをカバーする仮想画面サイズを計算
				minX, minY := screens.ScreenInfo[0].XOrg, screens.ScreenInfo[0].YOrg
				maxX := screens.ScreenInfo[0].XOrg + int16(screens.ScreenInfo[0].Width)
				maxY := screens.ScreenInfo[0].YOrg + int16(screens.ScreenInfo[0].Height)

				for _, screen := range screens.ScreenInfo[1:] {
					if screen.XOrg < minX {
						minX = screen.XOrg
					}
					if screen.YOrg < minY {
						minY = screen.YOrg
					}
					rightEdge := screen.XOrg + int16(screen.Width)
					bottomEdge := screen.YOrg + int16(screen.Height)
					if rightEdge > maxX {
						maxX = rightEdge
					}
					if bottomEdge > maxY {
						maxY = bottomEdge
					}
				}

				totalWidth := int(maxX - minX)
				totalHeight := int(maxY - minY)
				slog.Info("Xinerama検出", "screens", len(screens.ScreenInfo), "total_width", totalWidth, "total_height", totalHeight)
				return totalWidth, totalHeight
			}
		}
	}

	// Xineramaが使えない場合はデフォルトスクリーンを使用
	setup := xproto.Setup(r.xConn)
	screen := setup.DefaultScreen(r.xConn)
	slog.Info("デフォルトスクリーン使用", "width", screen.WidthInPixels, "height", screen.HeightInPixels)
	return int(screen.WidthInPixels), int(screen.HeightInPixels)
}

func (r *Ruler) setupClickThrough() error {
	return r.setupClickThroughForWindows(r.windows)
}

func (r *Ruler) setupClickThroughForWindows(windows []*xwindow.Window) error {
	extension, err := xproto.QueryExtension(r.xConn, uint16(len(extensionXFIXES)), extensionXFIXES).Reply()
	if err != nil || !extension.Present {
		return err
	}

	if err := xfixes.Init(r.xConn); err != nil {
		return err
	}

	if _, err := xfixes.QueryVersion(r.xConn, xfixesMajor, xfixesMinor).Reply(); err != nil {
		return err
	}

	region, err := xfixes.NewRegionId(r.xConn)
	if err != nil {
		return err
	}
	defer xfixes.DestroyRegion(r.xConn, region)

	if err := xfixes.CreateRegionChecked(r.xConn, region, []xproto.Rectangle{{}}).Check(); err != nil {
		return err
	}

	for _, win := range windows {
		winID := xproto.Window(win.Id)
		if err := xfixes.SetWindowShapeRegionChecked(r.xConn, winID, shape.SkInput, 0, 0, region).Check(); err != nil {
			return err
		}
	}

	return nil
}

func (r *Ruler) setupTransparency() error {
	return r.setupTransparencyForWindows(r.windows)
}

func (r *Ruler) setupTransparencyForWindows(windows []*xwindow.Window) error {
	atom, err := xproto.InternAtom(r.xConn, true, uint16(len(atomOpacity)), atomOpacity).Reply()
	if err != nil {
		return err
	}

	opacityPercent := r.mode.GetOpacity()
	maxOpacity := float64(uint32(0xFFFFFFFF))
	opacity := opacityPercent / 100.0 * maxOpacity
	opacityValue := uint32(opacity)

	opacityBytes := []byte{
		byte(opacityValue & 0xFF),
		byte((opacityValue >> 8) & 0xFF),
		byte((opacityValue >> 16) & 0xFF),
		byte((opacityValue >> 24) & 0xFF),
	}

	for _, win := range windows {
		winID := xproto.Window(win.Id)
		if err := xproto.ChangePropertyChecked(
			r.xConn,
			xproto.PropModeReplace,
			winID,
			atom.Atom,
			xproto.AtomCardinal,
			32,
			1,
			opacityBytes,
		).Check(); err != nil {
			return err
		}
	}

	return nil
}

func (r *Ruler) getCursor() (int, int, error) {
	setup := xproto.Setup(r.xConn)
	root := setup.DefaultScreen(r.xConn).Root

	reply, err := xproto.QueryPointer(r.xConn, root).Reply()
	if err != nil {
		return 0, 0, err
	}

	return int(reply.RootX), int(reply.RootY), nil
}
