package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func isOfficialSource(source string) bool {
	return strings.Contains(strings.TrimSpace(source), "官网")
}

func isTerminal(app Application) bool {
	if boolValue(app, "archived") {
		return true
	}
	stage := strings.ToLower(strings.TrimSpace(str(app, "current_stage")))
	status := strings.ToLower(strings.TrimSpace(str(app, "current_status")))
	outcome := strings.ToLower(strings.TrimSpace(str(app, "outcome")))
	failure := strings.TrimSpace(str(app, "failure_stage"))
	if stage == "offer" || stage == "结束" || failure != "" {
		return true
	}
	for _, word := range []string{"rejected", "reject", "failed", "closed", "withdrawn", "淘汰", "拒绝", "已挂", "结束", "挂了"} {
		if strings.Contains(status, word) || strings.Contains(outcome, word) {
			return true
		}
	}
	return outcome == "offer" || strings.HasPrefix(outcome, "offer ")
}

func officialURL(app Application) string {
	for _, key := range []string{"official_check_url", "source_url", "jd_url"} {
		if value := strings.TrimSpace(str(app, key)); value != "" {
			return value
		}
	}
	return ""
}

func duplicateGroups(apps []Application) []map[string]any {
	groups := map[string][]Application{}
	for _, app := range apps {
		company := normalize(str(app, "company"))
		role := normalize(str(app, "role"))
		if company == "" || role == "" {
			continue
		}
		key := company + "|" + role
		groups[key] = append(groups[key], app)
	}
	out := []map[string]any{}
	for key, items := range groups {
		if len(items) > 1 {
			out = append(out, map[string]any{"key": key, "count": len(items), "applications": items})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["count"].(int) > out[j]["count"].(int)
	})
	return out
}

func analytics(state State) map[string]any {
	total, active, interviews, offers := 0, 0, 0, 0
	channels := map[string]int{}
	resumes := map[string]int{}
	failures := map[string]int{}
	funnel := map[string]int{"已投递": 0, "笔试": 0, "一面": 0, "二面": 0, "三面": 0, "HR 面": 0, "Offer": 0}
	trend := map[string]int{}

	resumeNames := map[int]string{}
	for _, rv := range state.ResumeVersions {
		resumeNames[intValue(rv, "id")] = str(rv, "name")
	}

	for _, app := range state.Applications {
		if boolValue(app, "archived") {
			continue
		}
		total++
		if !isTerminal(app) {
			active++
		}
		stage := str(app, "current_stage")
		high := str(app, "highest_stage")
		if stageRank(stage) >= stageRank("一面") || stageRank(high) >= stageRank("一面") {
			interviews++
		}
		if strings.EqualFold(stage, "Offer") || strings.EqualFold(str(app, "outcome"), "Offer") {
			offers++
		}
		if source := str(app, "source"); source != "" {
			channels[source]++
		}
		if rid := intValue(app, "resume_version_id"); rid != 0 {
			name := resumeNames[rid]
			if name == "" {
				name = fmt.Sprintf("简历#%d", rid)
			}
			resumes[name]++
		}
		if failure := str(app, "failure_stage"); failure != "" {
			failures[failure]++
		}
		maxRank := stageRank(high)
		if maxRank < stageRank(stage) {
			maxRank = stageRank(stage)
		}
		for label := range funnel {
			if maxRank >= stageRank(label) {
				funnel[label]++
			}
		}
		if day := str(app, "applied"); len(day) >= 10 {
			trend[day[:10]]++
		}
	}

	return map[string]any{
		"total":          total,
		"active":         active,
		"interviews":     interviews,
		"offers":         offers,
		"interview_rate": rate(interviews, total),
		"offer_rate":     rate(offers, total),
		"channels":       channels,
		"resumes":        resumes,
		"failures":       failures,
		"funnel":         funnel,
		"trend":          trend,
	}
}

func rate(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) * 100 / float64(d)
}

func stageRank(stage string) int {
	stage = strings.TrimSpace(stage)
	ranks := map[string]int{"待投递": 0, "已投递": 1, "笔试": 2, "一面": 3, "二面": 4, "三面": 5, "HR 面": 6, "Offer": 7, "结束": -1}
	if rank, ok := ranks[stage]; ok {
		return rank
	}
	return 1
}

func exportGPT(apps []Application, ids map[int]bool) string {
	var b strings.Builder
	b.WriteString("# 求职数据导出\n\n")
	selected := 0
	for _, app := range apps {
		id := intValue(app, "id")
		if len(ids) > 0 && !ids[id] {
			continue
		}
		selected++
		fmt.Fprintf(&b, "## %s - %s\n\n", str(app, "company"), str(app, "role"))
		fmt.Fprintf(&b, "- ID: %d\n- 地点: %s\n- 渠道: %s\n- 投递日期: %s\n- 当前进度: %s\n- 最高进度: %s\n- 优先级: %s\n- 岗位方向: %s\n- 失败阶段: %s\n- 薪资: %s\n- 官网下次查询: %s\n- 官网查询次数: %d\n\n",
			id, str(app, "city"), str(app, "source"), str(app, "applied"), str(app, "current_stage"), str(app, "highest_stage"), str(app, "priority"), str(app, "job_direction"), str(app, "failure_stage"), str(app, "salary"), str(app, "next_official_check_at"), intValue(app, "official_check_count"))
	}
	fmt.Fprintf(&b, "---\n共 %d 条记录。\n", selected)
	return b.String()
}

func parseIDs(value string) map[int]bool {
	out := map[int]bool{}
	for _, part := range strings.Split(value, ",") {
		if id, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			out[id] = true
		}
	}
	return out
}

func normalize(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Join(strings.Fields(value), "")
	for _, token := range []string{"有限公司", "有限责任公司", "股份有限公司", "（", "）", "(", ")", "-", "_", " "} {
		value = strings.ReplaceAll(value, token, "")
	}
	return value
}

func decodeMap(r *http.Request) (Application, error) {
	m, err := decodeGenericMap(r)
	return Application(m), err
}

func decodeGenericMap(r *http.Request) (map[string]any, error) {
	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		return nil, errors.New("Content-Type must be application/json")
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return out, nil
}

func cloneMap[T ~map[string]any](in T) T {
	raw, _ := json.Marshal(in)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out T
	_ = decoder.Decode(&out)
	return out
}

func str(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func boolValue(m map[string]any, key string) bool {
	v := m[key]
	switch b := v.(type) {
	case bool:
		return b
	case string:
		parsed, _ := strconv.ParseBool(b)
		return parsed
	default:
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
