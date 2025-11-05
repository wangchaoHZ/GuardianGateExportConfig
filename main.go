package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"gopkg.in/yaml.v3"
)

/* ---------- 数据结构 (保持原样，无 ID) ---------- */

type Register struct {
	Collector string `yaml:"collector" json:"collector"`
	RegType   string `yaml:"reg_type" json:"reg_type"`
	Addr      int    `yaml:"addr" json:"addr"`
	ZhName    string `yaml:"zh_name" json:"zh_name"`
	EnName    string `yaml:"en_name" json:"en_name"`
	Unit      string `yaml:"unit" json:"unit"`
	Decimals  int    `yaml:"decimals" json:"decimals"`
	Type      string `yaml:"type" json:"type"`
	Length    int    `yaml:"length" json:"length"`
}
type InfluxDB struct {
	InfluxURL         string `yaml:"influx_url" json:"influx_url"`
	InfluxToken       string `yaml:"influx_token" json:"influx_token"`
	InfluxOrg         string `yaml:"influx_org" json:"influx_org"`
	InfluxBucket      string `yaml:"influx_bucket" json:"influx_bucket"`
	InfluxMeasurement string `yaml:"influx_measurement" json:"influx_measurement"`
}
type Config struct {
	WriteMode                 string     `yaml:"write_mode" json:"write_mode"`
	PeriodMs                  int        `yaml:"period_ms" json:"period_ms"`
	PerSecondSampleIntervalMs int        `yaml:"per_second_sample_interval_ms" json:"per_second_sample_interval_ms"`
	PerSecondFallbackPolicy   string     `yaml:"per_second_fallback_policy" json:"per_second_fallback_policy"`
	PerSecondTimestampEdge    string     `yaml:"per_second_timestamp_edge" json:"per_second_timestamp_edge"`
	InfluxDB                  InfluxDB   `yaml:"influxdb" json:"influxdb"`
	Registers                 []Register `yaml:"registers" json:"registers"`
}

type EditConfig struct {
	TargetPath     string `yaml:"target_yaml_path"`
	ExportPath     string `yaml:"export_yaml_path"` // 新增：导出文件路径（可绝对或相对）
	RestartService string `yaml:"restart_service"`
	RestartCommand string `yaml:"restart_command"`
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
	exportSelection map[string]struct{} // key: collector|reg_type|addr
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
	// 若未配置 ExportPath 则默认同目录 export.yaml
	if strings.TrimSpace(s.editConf.ExportPath) == "" {
		return filepath.Join(filepath.Dir(targetAbs), "export.yaml")
	}
	raw := normalizePath(s.editConf.ExportPath)
	// 如果不是绝对路径，则以目标文件目录作为基准
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(filepath.Dir(targetAbs), raw)
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		log.Printf("[WARN] ExportPath 解析失败，使用默认路径: %v", err)
		return filepath.Join(filepath.Dir(targetAbs), "export.yaml")
	}
	// 创建目录
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[WARN] 创建导出目录失败(%s): %v，回退默认路径", dir, err)
		return filepath.Join(filepath.Dir(targetAbs), "export.yaml")
	}
	// 防止与原文件同名（如果用户把 export 指向同一个文件是不合理的）
	if abs == targetAbs {
		log.Printf("[WARN] export_yaml_path 与 target_yaml_path 相同，回退为同目录 export.yaml")
		return filepath.Join(filepath.Dir(targetAbs), "export.yaml")
	}
	return abs
}

func (s *Service) openTarget() {
	if s.editConf.TargetPath == "" {
		s.lastErr = "edit_config.yaml 中 target_yaml_path 未配置"
		return
	}
	target := normalizePath(s.editConf.TargetPath)
	abs, err := filepath.Abs(target)
	if err != nil {
		s.lastErr = fmt.Sprintf("目标路径解析失败: %v", err)
		return
	}
	if _, err := os.Stat(abs); os.IsNotExist(err) {
		s.lastErr = fmt.Sprintf("目标文件不存在: %s", abs)
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		s.lastErr = fmt.Sprintf("读取目标文件失败: %v", err)
		return
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		s.lastErr = fmt.Sprintf("YAML 解析失败: %v", err)
		return
	}
	if err := validateConfig(&cfg); err != nil {
		s.lastErr = fmt.Errorf("配置校验失败: %v", err).Error()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = cfg
	s.version = int(time.Now().Unix())
	s.path = abs
	s.loaded = true
	s.lastErr = ""
	s.exportFilePath = s.resolveExportPath(abs)
	s.loadSelectionFileLocked()
	s.pruneSelectionLocked()
	_ = s.writeExportFileLocked()
	log.Printf("[INFO] 成功加载: %s version=%d export=%s", s.path, s.version, s.exportFilePath)
}

/* ---------- 校验 ---------- */
func validateConfig(cfg *Config) error {
	typeLen := map[string]int{"int16": 1, "uint16": 1, "float32": 2}
	seen := make(map[string]map[int]struct{})
	for i, r := range cfg.Registers {
		c := strings.TrimSpace(r.Collector)
		if c == "" {
			return fmt.Errorf("行 %d: 所属设备(collector) 不能为空", i)
		}
		rt := strings.TrimSpace(r.RegType)
		if rt == "" {
			return fmt.Errorf("行 %d: 寄存器类型(reg_type) 不能为空", i)
		}
		if r.Addr <= 0 || r.Addr > 65535 {
			return fmt.Errorf("行 %d: addr 超范围(1..65535)", i)
		}
		if r.Decimals < 0 {
			return fmt.Errorf("行 %d: decimals 不能为负", i)
		}
		if exp, ok := typeLen[r.Type]; ok && r.Length != exp {
			return fmt.Errorf("行 %d: 类型 %s 需要 length=%d 实际=%d", i, r.Type, exp, r.Length)
		}
		if _, ok := seen[c]; !ok {
			seen[c] = make(map[int]struct{})
		}
		if _, dup := seen[c][r.Addr]; dup {
			return fmt.Errorf("行 %d: 同设备 %s 地址 %d 重复", i, c, r.Addr)
		}
		seen[c][r.Addr] = struct{}{}
	}
	return nil
}

/* ---------- 保存 ---------- */
func (s *Service) saveUnsafe() error {
	if !s.loaded {
		return fmt.Errorf("尚未成功加载文件")
	}
	out, err := yaml.Marshal(s.config)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0644); err != nil {
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
			return
		}
		log.Printf("[WARN] 读取导出选择文件失败: %v", err)
		return
	}
	var sd selectionData
	if err := json.Unmarshal(data, &sd); err != nil {
		log.Printf("[WARN] 解析选择文件失败: %v", err)
		return
	}
	m := make(map[string]struct{})
	for _, id := range sd.Selected {
		m[id] = struct{}{}
	}
	s.exportSelection = m
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

func (s *Service) buildExportSubsetLocked() Config {
	var out Config
	out.WriteMode = s.config.WriteMode
	out.PeriodMs = s.config.PeriodMs
	out.PerSecondSampleIntervalMs = s.config.PerSecondSampleIntervalMs
	out.PerSecondFallbackPolicy = s.config.PerSecondFallbackPolicy
	out.PerSecondTimestampEdge = s.config.PerSecondTimestampEdge
	out.InfluxDB = s.config.InfluxDB
	for _, r := range s.config.Registers {
		if _, ok := s.exportSelection[regKey(&r)]; ok {
			out.Registers = append(out.Registers, r)
		}
	}
	return out
}

func (s *Service) writeExportFileLocked() error {
	sub := s.buildExportSubsetLocked()
	data, err := yaml.Marshal(sub)
	if err != nil {
		return err
	}
	if s.exportFilePath == "" {
		return fmt.Errorf("exportFilePath 未设置")
	}
	tmp := s.exportFilePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
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
		"targetPath":   s.editConf.TargetPath,
		"export_count": len(s.exportSelection),
		"export_file":  s.exportFilePath,
	})
}

func (s *Service) getConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.loaded {
		writeErr(w, 400, "未成功加载目标文件: "+s.lastErr)
		return
	}
	writeJSON(w, 200, map[string]any{
		"version": s.version,
		"config":  s.config,
	})
}

func (s *Service) putConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		writeErr(w, 400, "未成功加载文件: "+s.lastErr)
		return
	}
	var payload struct {
		Version int    `json:"version"`
		Config  Config `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeErr(w, 400, "JSON 解析失败: "+err.Error())
		return
	}
	if payload.Version != s.version {
		writeErr(w, 409, fmt.Sprintf("版本不匹配 server=%d client=%d", s.version, payload.Version))
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
	_ = s.writeExportFileLocked()
	writeJSON(w, 200, map[string]any{
		"version":      s.version,
		"config":       s.config,
		"export_keys":  s.exportKeysLocked(),
		"export_count": len(s.exportSelection),
		"export_file":  s.exportFilePath,
	})
}

func (s *Service) addRegister(w http.ResponseWriter, r *http.Request) {
	if !s.loaded {
		writeErr(w, 400, "未成功加载文件")
		return
	}
	var reg Register
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		writeErr(w, 400, "JSON 解析失败: "+err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp := s.config
	tmp.Registers = append(tmp.Registers, reg)
	if err := validateConfig(&tmp); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.config = tmp
	s.version++
	if err := s.saveUnsafe(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	_ = s.writeExportFileLocked()
	writeJSON(w, 201, map[string]any{"version": s.version, "config": s.config, "export_file": s.exportFilePath})
}

func (s *Service) deleteRegister(w http.ResponseWriter, r *http.Request) {
	if !s.loaded {
		writeErr(w, 400, "未成功加载文件")
		return
	}
	idxStr := chi.URLParam(r, "index")
	var idx int
	if _, err := fmt.Sscanf(idxStr, "%d", &idx); err != nil {
		writeErr(w, 400, "index 无效")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < 0 || idx >= len(s.config.Registers) {
		writeErr(w, 404, "index 越界")
		return
	}
	removedKey := regKey(&s.config.Registers[idx])
	tmp := s.config
	tmp.Registers = append(tmp.Registers[:idx], tmp.Registers[idx+1:]...)
	if err := validateConfig(&tmp); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.config = tmp
	s.version++
	if err := s.saveUnsafe(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if _, ok := s.exportSelection[removedKey]; ok {
		delete(s.exportSelection, removedKey)
		s.writeSelectionFileLocked()
	}
	_ = s.writeExportFileLocked()
	writeJSON(w, 200, map[string]any{"version": s.version, "rows": len(s.config.Registers), "export_file": s.exportFilePath})
}

func (s *Service) exportAllYAML(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.loaded {
		writeErr(w, 400, "未成功加载文件")
		return
	}
	data, _ := yaml.Marshal(s.config)
	ts := time.Now().Format("20060102_150405")
	filename := fmt.Sprintf("registers_full_%s.yaml", ts)
	w.Header().Set("Content-Type", "application/x-yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func (s *Service) exportSubsetYAML(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		writeErr(w, 400, "未成功加载文件")
		return
	}
	if err := s.writeExportFileLocked(); err != nil {
		writeErr(w, 500, "生成 export.yaml 失败: "+err.Error())
		return
	}
	data, err := os.ReadFile(s.exportFilePath)
	if err != nil {
		writeErr(w, 500, "读取 export.yaml 失败: "+err.Error())
		return
	}
	ts := time.Now().Format("20060102_150405")
	filename := fmt.Sprintf("export_subset_%s.yaml", ts)
	w.Header().Set("Content-Type", "application/x-yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func (s *Service) getExportSelection(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, 200, map[string]any{
		"selected": s.exportKeysLocked(),
		"count":    len(s.exportSelection),
	})
}

func (s *Service) putExportSelection(w http.ResponseWriter, r *http.Request) {
	var payload selectionData
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeErr(w, 400, "JSON 解析失败: "+err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		writeErr(w, 400, "未成功加载文件")
		return
	}
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
	_ = s.writeExportFileLocked()
	writeJSON(w, 200, map[string]any{
		"selected":    s.exportKeysLocked(),
		"count":       len(s.exportSelection),
		"export_file": s.exportFilePath,
	})
}

func (s *Service) deleteExportSelection(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.exportSelection[key]; ok {
		delete(s.exportSelection, key)
		s.writeSelectionFileLocked()
		_ = s.writeExportFileLocked()
		writeJSON(w, 200, map[string]string{"status": "removed", "key": key, "export_file": s.exportFilePath})
		return
	}
	writeErr(w, 404, "key 不在选择中")
}

func (s *Service) diff(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.loaded {
		writeErr(w, 400, "未成功加载文件")
		return
	}
	since := r.URL.Query().Get("since")
	writeJSON(w, 200, map[string]string{
		"message":        "Diff 占位（未实现审计）",
		"since_version":  since,
		"currentVersion": fmt.Sprintf("%d", s.version),
	})
}

func (s *Service) reload(w http.ResponseWriter, r *http.Request) {
	s.openTarget()
	if !s.loaded {
		writeErr(w, 400, "重新加载失败: "+s.lastErr)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.writeExportFileLocked()
	writeJSON(w, 200, map[string]any{
		"message":     "重新加载成功",
		"version":     s.version,
		"rows":        len(s.config.Registers),
		"path":        s.path,
		"export_file": s.exportFilePath,
	})
}

/* ---------- 重启服务 ---------- */

func (s *Service) restartService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "Method Not Allowed")
		return
	}
	cmdLine := strings.TrimSpace(s.editConf.RestartCommand)
	if cmdLine != "" {
		go runShellCommand(cmdLine)
		writeJSON(w, 200, map[string]any{"status": "triggered", "detail": "执行自定义命令"})
		return
	}
	svc := strings.TrimSpace(s.editConf.RestartService)
	if svc == "" {
		writeErr(w, 400, "未配置 restart_service 或 restart_command")
		return
	}
	go func(name string) {
		cmd := exec.Command("systemctl", "restart", name)
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("[WARN] restart %s 失败: %v output=%s", name, err, string(out))
		} else {
			log.Printf("[INFO] restart %s OK: %s", name, string(out))
		}
	}(svc)
	writeJSON(w, 200, map[string]any{"status": "triggered", "detail": "systemctl restart " + svc})
}

func runShellCommand(cmdLine string) {
	parts := strings.Fields(cmdLine)
	if len(parts) == 0 {
		return
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[WARN] restart command 失败: %v output=%s", err, out)
	} else {
		log.Printf("[INFO] restart command OK output=%s", out)
	}
}

/* ---------- 工具 ---------- */
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

/* ---------- main ---------- */
func main() {
	svc := NewService()
	svc.loadEditConfig()
	svc.openTarget()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "static/index.html")
	})

	// 基础配置
	r.Get("/api/status", svc.status)
	r.Get("/api/config", svc.getConfig)
	r.Put("/api/config", svc.putConfig)
	r.Post("/api/register", svc.addRegister)
	r.Delete("/api/register/{index}", svc.deleteRegister)
	r.Post("/api/reload", svc.reload)
	r.Post("/api/restart-service", svc.restartService)

	// 导出（全部 / 子集）
	r.Get("/api/export/yaml", svc.exportAllYAML)
	r.Get("/api/export/subset.yaml", svc.exportSubsetYAML)

	// 选择管理
	r.Get("/api/export/selection", svc.getExportSelection)
	r.Put("/api/export/selection", svc.putExportSelection)
	r.Delete("/api/export/selection/{key}", svc.deleteExportSelection)

	// Diff 占位
	r.Get("/api/diff", svc.diff)

	log.Println("Goto: http://localhost:8089/")
	log.Fatal(http.ListenAndServe(":8089", r))
}
