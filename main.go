package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/xuri/excelize/v2"
	"gopkg.in/yaml.v3"
)

/* ---------- 数据结构 ---------- */

type Register struct {
	Collector string `json:"collector"`
	RegType   string `json:"reg_type"`
	Addr      int    `json:"addr"`
	ZhName    string `json:"zh_name"`
	EnName    string `json:"en_name"`
	Unit      string `json:"unit"`
	Decimals  int    `json:"decimals"`
	Type      string `json:"type"`
	Length    int    `json:"length"`
}

type Config struct {
	Registers []Register `json:"registers"`
}

type EditConfig struct {
	TargetExcelPath string `yaml:"target_excel_path"`
	ExportExcelPath string `yaml:"export_excel_path"`
	RestartService  string `yaml:"restart_service"`
	RestartCommand  string `yaml:"restart_command"`
}

/* ---------- 导出选择文件结构 ---------- */

type selectionData struct {
	Selected []string `json:"selected"`
}

/* ---------- Service ---------- */

type Service struct {
	mu              sync.RWMutex
	loaded          bool
	version         int
	path            string
	config          Config
	lastErr         string
	editConf        EditConfig
	selectionPath   string
	exportFilePath  string
	exportSelection map[string]struct{}
}

func NewService() *Service {
	return &Service{
		selectionPath:   "export_selection.json",
		exportSelection: make(map[string]struct{}),
	}
}

func (s *Service) loadEditConfig() {
	data, err := os.ReadFile("edit_config.yaml")
	if err != nil {
		s.lastErr = fmt.Sprintf("读取 edit_config.yaml 失败: %v", err)
		return
	}
	if err := yaml.Unmarshal(data, &s.editConf); err != nil {
		s.lastErr = fmt.Sprintf("解析 edit_config.yaml 失败: %v", err)
		return
	}
}

func normalizePath(p string) string { return strings.ReplaceAll(p, "\\", "/") }

func (s *Service) resolveExportPath(targetAbs string) string {
	raw := s.editConf.ExportExcelPath
	if strings.TrimSpace(raw) == "" {
		return filepath.Join(filepath.Dir(targetAbs), "export_registers.xlsx")
	}
	raw = normalizePath(raw)
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(filepath.Dir(targetAbs), raw)
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		log.Printf("[WARN] ExportPath 解析失败，使用默认路径: %v", err)
		return filepath.Join(filepath.Dir(targetAbs), "export_registers.xlsx")
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[WARN] 创建导出目录失败(%s): %v，回退默认路径", dir, err)
		return filepath.Join(filepath.Dir(targetAbs), "export_registers.xlsx")
	}
	return abs
}

// ===== 核心：从 Excel 加载寄存器 =====
func loadRegistersFromExcel(filename string) ([]Register, error) {
	f, err := excelize.OpenFile(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sheetName := f.GetSheetName(0)
	if sheetName == "" {
		sheetName = "Sheet1"
	}

	rows, err := f.GetRows(sheetName)
	if err != nil {
		return nil, err
	}

	var regs []Register
	for i, row := range rows {
		if i == 0 {
			continue
		} // 跳过表头
		for len(row) < 9 {
			row = append(row, "")
		}
		if row[0] == "" && row[2] == "" {
			continue
		}

		addr, _ := strconv.Atoi(row[2])
		decimals, _ := strconv.Atoi(row[6])
		length, _ := strconv.Atoi(row[8])
		if length == 0 {
			length = 1
		}

		regs = append(regs, Register{
			Collector: row[0],
			RegType:   row[1],
			Addr:      addr,
			ZhName:    row[3],
			EnName:    row[4],
			Unit:      row[5],
			Decimals:  decimals,
			Type:      row[7],
			Length:    length,
		})
	}
	return regs, nil
}

// ===== 核心：将寄存器写入 Excel =====
func buildExcel(regs []Register) (*excelize.File, error) {
	f := excelize.NewFile()
	sheet := "Sheet1"
	headers := []interface{}{"Collector", "RegType", "Addr", "ZhName", "EnName", "Unit", "Decimals", "Type", "Length"}
	_ = f.SetSheetRow(sheet, "A1", &headers)

	for i, r := range regs {
		row := []interface{}{
			r.Collector, r.RegType, r.Addr, r.ZhName, r.EnName, r.Unit, r.Decimals, r.Type, r.Length,
		}
		axis, _ := excelize.CoordinatesToCellName(1, i+2)
		_ = f.SetSheetRow(sheet, axis, &row)
	}
	return f, nil
}

func (s *Service) openTarget() {
	if s.editConf.TargetExcelPath == "" {
		s.lastErr = "edit_config.yaml 中 target_excel_path 未配置"
		return
	}
	target := normalizePath(s.editConf.TargetExcelPath)
	abs, err := filepath.Abs(target)
	if err != nil {
		s.lastErr = fmt.Sprintf("目标路径解析失败: %v", err)
		return
	}

	// 如果文件不存在，自动创建一个空的模板
	if _, err := os.Stat(abs); os.IsNotExist(err) {
		log.Printf("[INFO] 源文件不存在，自动创建模板: %s", abs)
		emptyF, _ := buildExcel([]Register{})
		_ = emptyF.SaveAs(abs)
	}

	regs, err := loadRegistersFromExcel(abs)
	if err != nil {
		s.lastErr = fmt.Sprintf("读取 Excel 失败: %v", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.Registers = regs
	s.version = int(time.Now().Unix())
	s.path = abs
	s.loaded = true
	s.lastErr = ""
	s.exportFilePath = s.resolveExportPath(abs)
	s.loadSelectionFileLocked()
	s.pruneSelectionLocked()
	_ = s.writeExportExcelLocked()
	log.Printf("[INFO] 成功加载 Excel: %s (行数: %d)", s.path, len(regs))
}

/* ---------- 校验 ---------- */
func validateConfig(cfg *Config) error {
	typeLen := map[string]int{"int16": 1, "uint16": 1, "float32": 2}
	seen := make(map[string]map[int]struct{})
	for i, r := range cfg.Registers {
		c := strings.TrimSpace(r.Collector)
		if c == "" {
			return fmt.Errorf("行 %d: 设备不能为空", i+1)
		}
		if r.Addr <= 0 || r.Addr > 65535 {
			return fmt.Errorf("行 %d: addr超范围(1..65535)", i+1)
		}
		if exp, ok := typeLen[r.Type]; ok && r.Length != exp {
			return fmt.Errorf("行 %d: 类型 %s 需要 length=%d 实际=%d", i+1, r.Type, exp, r.Length)
		}
		if _, ok := seen[c]; !ok {
			seen[c] = make(map[int]struct{})
		}
		if _, dup := seen[c][r.Addr]; dup {
			return fmt.Errorf("行 %d: 设备 %s 地址 %d 重复", i+1, c, r.Addr)
		}
		seen[c][r.Addr] = struct{}{}
	}
	return nil
}

/* ---------- 保存 ---------- */
func (s *Service) saveUnsafe() error {
	if !s.loaded {
		return fmt.Errorf("尚未加载文件")
	}
	f, err := buildExcel(s.config.Registers)
	if err != nil {
		return err
	}
	defer f.Close()

	// 【修改点】：临时文件以 .tmp.xlsx 结尾，骗过 excelize 的格式校验
	tmp := s.path + ".tmp.xlsx"
	if err := f.SaveAs(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

/* ---------- 导出选择逻辑 ---------- */

func regKey(r *Register) string {
	return fmt.Sprintf("%s|%s|%d", r.Collector, r.RegType, r.Addr)
}

func (s *Service) loadSelectionFileLocked() {
	data, err := os.ReadFile(s.selectionPath)
	if err != nil {
		if os.IsNotExist(err) {
			_ = os.WriteFile(s.selectionPath, []byte(`{"selected":[]}`), 0644)
		}
		return
	}
	var sd selectionData
	if err := json.Unmarshal(data, &sd); err == nil {
		m := make(map[string]struct{})
		for _, id := range sd.Selected {
			m[id] = struct{}{}
		}
		s.exportSelection = m
	}
}

func (s *Service) writeSelectionFileLocked() {
	sd := selectionData{Selected: s.exportKeysLocked()}
	b, _ := json.MarshalIndent(sd, "", "  ")
	_ = os.WriteFile(s.selectionPath, b, 0644)
}

func (s *Service) exportKeysLocked() []string {
	out := make([]string, 0, len(s.exportSelection))
	for k := range s.exportSelection {
		out = append(out, k)
	}
	return out
}

func (s *Service) pruneSelectionLocked() {
	valid := make(map[string]struct{})
	for i := range s.config.Registers {
		valid[regKey(&s.config.Registers[i])] = struct{}{}
	}
	changed := false
	for k := range s.exportSelection {
		if _, ok := valid[k]; !ok {
			delete(s.exportSelection, k)
			changed = true
		}
	}
	if changed {
		s.writeSelectionFileLocked()
	}
}

func (s *Service) writeExportExcelLocked() error {
	var exportRegs []Register
	for _, r := range s.config.Registers {
		if _, ok := s.exportSelection[regKey(&r)]; ok {
			exportRegs = append(exportRegs, r)
		}
	}

	f, err := buildExcel(exportRegs)
	if err != nil {
		return err
	}
	defer f.Close()

	// 【修改点】：临时文件以 .tmp.xlsx 结尾
	tmp := s.exportFilePath + ".tmp.xlsx"
	if err := f.SaveAs(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, s.exportFilePath)
}

/* ---------- HTTP Handlers ---------- */

func (s *Service) status(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, 200, map[string]any{
		"loaded":       s.loaded,
		"version":      s.version,
		"path":         s.path,
		"rows":         len(s.config.Registers),
		"error":        s.lastErr,
		"targetPath":   s.editConf.TargetExcelPath,
		"export_count": len(s.exportSelection),
		"export_file":  s.exportFilePath,
	})
}

func (s *Service) getConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.loaded {
		writeErr(w, 400, "加载失败: "+s.lastErr)
		return
	}
	writeJSON(w, 200, map[string]any{"version": s.version, "config": s.config})
}

func (s *Service) putConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		writeErr(w, 400, "加载失败: "+s.lastErr)
		return
	}

	var payload struct {
		Version int    `json:"version"`
		Config  Config `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeErr(w, 400, "解析失败: "+err.Error())
		return
	}
	if err := validateConfig(&payload.Config); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.config = payload.Config
	s.version++
	if err := s.saveUnsafe(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	s.pruneSelectionLocked()
	_ = s.writeExportExcelLocked()
	writeJSON(w, 200, map[string]any{
		"version": s.version, "config": s.config,
		"export_count": len(s.exportSelection), "export_file": s.exportFilePath,
	})
}

func (s *Service) addRegister(w http.ResponseWriter, r *http.Request) { /* 同原逻辑，省略重复代码...略微调整适配Config */
}
func (s *Service) deleteRegister(w http.ResponseWriter, r *http.Request) { /* 同原逻辑... */ }

func (s *Service) exportAllExcel(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.loaded {
		writeErr(w, 400, "未加载文件")
		return
	}
	data, _ := os.ReadFile(s.path)
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="registers_full_`+time.Now().Format("20060102")+`.xlsx"`)
	w.Write(data)
}

func (s *Service) exportSubsetExcel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		writeErr(w, 400, "未加载文件")
		return
	}
	_ = s.writeExportExcelLocked()
	data, _ := os.ReadFile(s.exportFilePath)
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="export_subset_`+time.Now().Format("20060102")+`.xlsx"`)
	w.Write(data)
}

func (s *Service) getExportSelection(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, 200, map[string]any{"selected": s.exportKeysLocked(), "count": len(s.exportSelection)})
}

func (s *Service) putExportSelection(w http.ResponseWriter, r *http.Request) {
	var payload selectionData
	_ = json.NewDecoder(r.Body).Decode(&payload)
	s.mu.Lock()
	defer s.mu.Unlock()
	valid := make(map[string]struct{})
	for _, r := range s.config.Registers {
		valid[regKey(&r)] = struct{}{}
	}
	newSel := make(map[string]struct{})
	for _, k := range payload.Selected {
		if _, ok := valid[k]; ok {
			newSel[k] = struct{}{}
		}
	}
	s.exportSelection = newSel
	s.writeSelectionFileLocked()
	_ = s.writeExportExcelLocked()
	writeJSON(w, 200, map[string]any{"selected": s.exportKeysLocked(), "count": len(s.exportSelection)})
}

func (s *Service) reload(w http.ResponseWriter, r *http.Request) {
	s.openTarget()
	writeJSON(w, 200, map[string]any{"message": "重新加载成功", "version": s.version})
}

/* ---------- 重启服务等辅助函数 (略微精简) ---------- */
func (s *Service) restartService(w http.ResponseWriter, r *http.Request) {
	if s.editConf.RestartService != "" {
		go exec.Command("systemctl", "restart", s.editConf.RestartService).Run()
	}
	writeJSON(w, 200, map[string]any{"status": "triggered"})
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	svc := NewService()
	svc.loadEditConfig()
	svc.openTarget()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, "static/index.html") })

	r.Get("/api/status", svc.status)
	r.Get("/api/config", svc.getConfig)
	r.Put("/api/config", svc.putConfig)
	r.Post("/api/reload", svc.reload)
	r.Post("/api/restart-service", svc.restartService)

	// 新增：下载 Excel 文件
	r.Get("/api/export/all.xlsx", svc.exportAllExcel)
	r.Get("/api/export/subset.xlsx", svc.exportSubsetExcel)

	r.Get("/api/export/selection", svc.getExportSelection)
	r.Put("/api/export/selection", svc.putExportSelection)

	log.Println("Editor Running on: http://localhost:8002/")
	log.Fatal(http.ListenAndServe(":8002", r))
}
