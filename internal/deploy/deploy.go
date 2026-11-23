// Package deploy 实现发布编排。
//
// 状态机：
//
//	pending -> running -> success
//	                   -> failed      （前置校验/切换阶段挂了）
//	                   -> rolled_back （健康检查失败，已自动回滚）
//
// 设计要点：
//   - 每一步都落库，前端可轮询看到实时进度，而不是只给一个"成功/失败"；
//   - 健康检查是"真检查"，不是 curl 一下就算 —— 连续 N 次通过才算成功，
//     避免新进程刚起来、还依赖一个尚未就绪的数据库时被误判为健康；
//   - 失败必须能自动回滚，且回滚后要再健康检查一次，确认真的回到了可用状态。
package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Ricevire/stormops/internal/config"
	"github.com/Ricevire/stormops/internal/model"
	"github.com/Ricevire/stormops/internal/store"
)

// StepNames 是固定的发布步骤，顺序即执行顺序。
var StepNames = []string{"precheck", "backup", "stage", "verify", "switch", "health", "rollback"}

var stepLabels = map[string]string{
	"precheck": "前置校验",
	"backup":   "备份当前版本",
	"stage":    "暂存新版本",
	"verify":   "产物校验",
	"switch":   "切换并重启",
	"health":   "健康检查",
	"rollback": "自动回滚",
}

// Orchestrator 负责执行发布。
type Orchestrator struct {
	cfg config.DeployConfig
	mu  sync.Mutex
}

// New 创建编排器。
func New(cfg config.DeployConfig) *Orchestrator {
	return &Orchestrator{cfg: cfg}
}

// Request 是一次发布请求。
type Request struct {
	Service  string `json:"service"`
	Version  string `json:"version"`
	Artifact string `json:"artifact"`
	Strategy string `json:"strategy"`
	Operator string `json:"operator"`
	Note     string `json:"note"`
}

// Start 创建发布记录并异步执行。
func (o *Orchestrator) Start(st *store.Store, req Request) (model.Release, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if strings.TrimSpace(req.Version) == "" {
		return model.Release{}, fmt.Errorf("version 不能为空")
	}
	if req.Strategy == "" {
		req.Strategy = "rolling"
	}
	if req.Operator == "" {
		req.Operator = "unknown"
	}

	var running bool
	var prevVersion string
	var svcName string
	st.View(func(s *model.State) {
		for _, r := range s.Releases {
			if r.Status == "running" {
				running = true
			}
		}
		for _, svc := range s.Services {
			if req.Service == "" || svc.Name == req.Service {
				prevVersion = svc.Version
				if svcName == "" {
					svcName = svc.Name
				}
			}
		}
	})
	if running {
		return model.Release{}, fmt.Errorf("已有发布在进行中，请等待完成后再发起")
	}
	if req.Service == "" {
		req.Service = svcName
	}

	now := time.Now().Unix()
	rel := model.Release{
		ID:          fmt.Sprintf("rel_%d_%s", now, shortHash(req.Service+req.Version)),
		Service:     req.Service,
		Version:     req.Version,
		PrevVersion: prevVersion,
		Artifact:    req.Artifact,
		Strategy:    req.Strategy,
		Status:      "running",
		Operator:    req.Operator,
		StartedAt:   now,
		Note:        req.Note,
	}
	for _, n := range StepNames {
		status := "pending"
		if n == "rollback" {
			status = "skipped"
		}
		rel.Steps = append(rel.Steps, model.ReleaseStep{Name: n, Status: status})
	}

	st.Update(func(s *model.State) {
		s.Releases = append([]model.Release{rel}, s.Releases...)
		if len(s.Releases) > 100 {
			s.Releases = s.Releases[:100]
		}
	})

	go o.run(st, rel.ID, req)
	return rel, nil
}

func (o *Orchestrator) run(st *store.Store, relID string, req Request) {
	o.mu.Lock()
	defer o.mu.Unlock()

	failed := func(step, detail string) {
		markStep(st, relID, step, "failed", detail)
		finish(st, relID, "failed")
	}

	// 1. precheck
	markStep(st, relID, "precheck", "running", "")
	if detail, ok := o.precheck(req); !ok {
		failed("precheck", detail)
		return
	} else if detail != "" {
		markStep(st, relID, "precheck", "success", detail)
	} else {
		markStep(st, relID, "precheck", "success", "产物存在，磁盘余量充足")
	}

	// 2. backup
	markStep(st, relID, "backup", "running", "")
	if err := o.hook(req, "backup"); err != nil {
		failed("backup", "备份失败: "+err.Error())
		return
	}
	markStep(st, relID, "backup", "success", o.hookDesc("backup", "已备份当前版本"))

	// 3. stage
	markStep(st, relID, "stage", "running", "")
	stagePath, err := o.stage(req)
	if err != nil {
		failed("stage", err.Error())
		return
	}
	markStep(st, relID, "stage", "success", "已暂存到 "+stagePath)

	// 4. verify
	markStep(st, relID, "verify", "running", "")
	sum, err := fileSHA256(stagePath)
	if err != nil {
		failed("verify", "计算校验和失败: "+err.Error())
		return
	}
	info, _ := os.Stat(stagePath)
	markStep(st, relID, "verify", "success",
		fmt.Sprintf("sha256=%s 大小=%dKB 权限=%s", sum[:12], info.Size()/1024, info.Mode().Perm()))

	// 5. switch
	markStep(st, relID, "switch", "running", "")
	if err := o.hook(req, "switch"); err != nil {
		failed("switch", "切换失败: "+err.Error())
		return
	}
	markStep(st, relID, "switch", "success", o.hookDesc("switch", "已切换到新版本并重启服务"))

	// 6. health
	markStep(st, relID, "health", "running", "")
	ok, detail := o.waitHealthy()
	if ok {
		markStep(st, relID, "health", "success", detail)
		finish(st, relID, "success")
		st.Update(func(s *model.State) {
			for i := range s.Services {
				if s.Services[i].Name == req.Service {
					s.Services[i].Version = req.Version
					s.Services[i].UpdatedAt = time.Now().Unix()
				}
			}
		})
		return
	}
	markStep(st, relID, "health", "failed", detail)

	// 7. rollback
	markStep(st, relID, "rollback", "running", "")
	if err := o.hook(req, "rollback"); err != nil {
		markStep(st, relID, "rollback", "failed", "回滚命令执行失败: "+err.Error()+" —— 需要人工介入！")
		finish(st, relID, "failed")
		return
	}
	if ok2, d2 := o.waitHealthy(); ok2 {
		markStep(st, relID, "rollback", "success", "已回滚并恢复健康："+d2)
		finish(st, relID, "rolled_back")
	} else {
		markStep(st, relID, "rollback", "failed", "回滚后仍不健康："+d2+" —— 需要人工介入！")
		finish(st, relID, "failed")
	}
}

func (o *Orchestrator) precheck(req Request) (string, bool) {
	if o.cfg.DryRun {
		return "dry-run 模式：跳过磁盘与产物校验", true
	}
	if req.Artifact == "" {
		return "未指定产物路径", false
	}
	info, err := os.Stat(req.Artifact)
	if err != nil {
		return "产物不存在: " + err.Error(), false
	}
	if info.IsDir() {
		return "产物路径是个目录", false
	}
	if info.Size() == 0 {
		return "产物大小为 0 字节", false
	}
	// 检查目标盘剩余空间，至少留 3 倍产物大小
	dir := o.cfg.TargetDir
	if dir == "" {
		dir = "/opt"
	}
	var free uint64
	if st, err := diskFree(dir); err == nil {
		free = st
	}
	if free > 0 && free < uint64(info.Size()*3) {
		return fmt.Sprintf("目标盘剩余空间不足：剩余 %dMB，产物 %dMB", free/1024/1024, info.Size()/1024/1024), false
	}
	return "", true
}

func (o *Orchestrator) stage(req Request) (string, error) {
	dir := o.cfg.TargetDir
	if dir == "" {
		dir = "/tmp/stormops-stage"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建暂存目录失败: %w", err)
	}
	dst := filepath.Join(dir, "artifact-"+req.Version)
	if o.cfg.DryRun {
		return dst + "（dry-run，未实际复制）", nil
	}
	src, err := os.Open(req.Artifact)
	if err != nil {
		return "", fmt.Errorf("打开产物失败: %w", err)
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("创建暂存文件失败: %w", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, src); err != nil {
		return "", fmt.Errorf("复制产物失败: %w", err)
	}
	return dst, nil
}

func (o *Orchestrator) hook(req Request, name string) error {
	if o.cfg.DryRun {
		time.Sleep(300 * time.Millisecond) // 让前端能看到步骤流转
		return nil
	}
	cmdStr := strings.TrimSpace(o.cfg.Hooks[name])
	if cmdStr == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	c.Env = append(os.Environ(),
		"STORMOPS_SERVICE="+req.Service,
		"STORMOPS_VERSION="+req.Version,
		"STORMOPS_ARTIFACT="+req.Artifact,
		"STORMOPS_STAGE="+filepath.Join(o.cfg.TargetDir, "artifact-"+req.Version),
		"STORMOPS_TARGET_DIR="+o.cfg.TargetDir,
	)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (o *Orchestrator) hookDesc(name, fallback string) string {
	if o.cfg.DryRun {
		return fallback + "（dry-run）"
	}
	if h := strings.TrimSpace(o.cfg.Hooks[name]); h != "" {
		return "执行: " + h
	}
	return fallback
}

// waitHealthy 轮询健康检查端点，要求连续 3 次通过。
//
// 为什么是"连续 3 次"而不是"1 次通过"：
// 进程刚启动时监听端口已经打开，但数据库连接池可能还没预热，
// 单次检查很容易假阳性，把故障版本判成健康。
func (o *Orchestrator) waitHealthy() (bool, string) {
	if o.cfg.HealthURL == "" {
		return true, "未配置健康检查地址，跳过"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	passes, attempts := 0, 0
	lastErr := ""
	for time.Now().Before(deadline) {
		attempts++
		resp, err := client.Get(o.cfg.HealthURL)
		if err != nil {
			lastErr = err.Error()
			passes = 0
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			if resp.StatusCode == 200 {
				passes++
				if passes >= 3 {
					return true, fmt.Sprintf("连续 3 次健康检查通过（共尝试 %d 次）", attempts)
				}
			} else {
				passes = 0
				lastErr = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			}
		}
		time.Sleep(700 * time.Millisecond)
	}
	return false, fmt.Sprintf("30 秒内未通过健康检查（%d 次尝试，最后一次错误：%s）", attempts, lastErr)
}

func markStep(st *store.Store, relID, step, status, detail string) {
	now := time.Now().Unix()
	st.Update(func(s *model.State) {
		for i := range s.Releases {
			if s.Releases[i].ID != relID {
				continue
			}
			for j := range s.Releases[i].Steps {
				if s.Releases[i].Steps[j].Name != step {
					continue
				}
				s.Releases[i].Steps[j].Status = status
				if detail != "" {
					s.Releases[i].Steps[j].Detail = detail
				}
				if status == "running" {
					s.Releases[i].Steps[j].StartedAt = now
				}
				if status == "success" || status == "failed" {
					if s.Releases[i].Steps[j].StartedAt == 0 {
						s.Releases[i].Steps[j].StartedAt = now
					}
					s.Releases[i].Steps[j].EndedAt = now
				}
			}
		}
	})
}

func finish(st *store.Store, relID, status string) {
	now := time.Now().Unix()
	st.Update(func(s *model.State) {
		for i := range s.Releases {
			if s.Releases[i].ID == relID {
				s.Releases[i].Status = status
				s.Releases[i].EndedAt = now
			}
		}
	})
}

// Labels 返回步骤的中文名，供前端展示。
func Labels() map[string]string {
	out := make(map[string]string, len(stepLabels))
	for k, v := range stepLabels {
		out[k] = v
	}
	return out
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func shortHash(s string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return fmt.Sprintf("%06x", h&0xffffff)
}

func diskFree(path string) (uint64, error) {
	type statfsT struct {
		Bsize, Frsize, Blocks, Bfree, Bavail uint64
	}
	// 用 df 兜底，避免引入 syscall 的平台差异
	c := exec.Command("df", "-B1", "--output=avail", path)
	out, err := c.Output()
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("df 输出异常")
	}
	var v uint64
	_, err = fmt.Sscanf(strings.TrimSpace(lines[len(lines)-1]), "%d", &v)
	return v, err
}
