package main

import (
	"reflect"
	"testing"
)

func TestPluginEventNamespaces(t *testing.T) {
	cases := []struct {
		name string
		want []string
	}{
		{"dsh-cost-meter", []string{"dsh-cost-meter", "cost-meter"}},
		{"cost-meter", []string{"cost-meter"}},
		{"@refyon/dsh-ui-taste", []string{"@refyon/dsh-ui-taste", "dsh-ui-taste", "ui-taste"}},
		{"restrict-discipline", []string{"restrict-discipline"}},
		{"  Dsh-Cost-Meter  ", []string{"dsh-cost-meter", "cost-meter"}},
		{"", nil},
		{"   ", nil},
	}
	for _, c := range cases {
		if got := pluginEventNamespaces(c.name); !reflect.DeepEqual(got, c.want) {
			t.Errorf("pluginEventNamespaces(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPluginEventNamespaceOf(t *testing.T) {
	cases := map[string]string{
		"cost-meter/native-search-usage": "cost-meter",
		"Cost-Meter/x":                   "cost-meter",
		"tool/result":                    "tool",
		"noSlash":                        "",
		"/leading":                       "",
	}
	for in, want := range cases {
		if got := pluginEventNamespaceOf(in); got != want {
			t.Errorf("pluginEventNamespaceOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// 归因：命名空间匹配插件包名（去 dsh- 前缀），会话数为并集（一个会话只算一次）。
func TestPluginRiskFromScan(t *testing.T) {
	scan := sessionEventScan{
		Stats: []pluginEventStat{
			{Type: "cost-meter/native-search-usage", Sessions: 2, Events: 30},
			{Type: "other-plugin/thing", Sessions: 1, Events: 5},
		},
		BySession: map[string][]string{
			"scope/a/session.v3.jsonl.zstd": {"cost-meter/native-search-usage"},
			"scope/b/session.v3.jsonl.zstd": {"cost-meter/native-search-usage", "other-plugin/thing"},
			"scope/c/session.v3.jsonl.zstd": {"other-plugin/thing"},
		},
	}
	risk := pluginRiskFromScan("dsh-cost-meter", scan)
	if risk == nil {
		t.Fatal("expected risk for dsh-cost-meter")
	}
	if risk.Sessions != 2 || risk.Events != 30 {
		t.Fatalf("risk = %+v, want sessions=2 events=30", risk)
	}
	if !reflect.DeepEqual(risk.Types, []string{"cost-meter/native-search-usage"}) {
		t.Fatalf("risk.Types = %v", risk.Types)
	}
	if other := pluginRiskFromScan("restrict-discipline", scan); other != nil {
		t.Fatalf("unrelated plugin should have no risk, got %+v", other)
	}
	if none := pluginRiskFromScan("dsh-cost-meter", sessionEventScan{}); none != nil {
		t.Fatalf("empty scan should yield no risk, got %+v", none)
	}
}

func TestParseSessionEventScan(t *testing.T) {
	out := "noise\n" +
		"DSHEVENTS\tSESSION\tscope/a/session.v3.jsonl.zstd\tcost-meter/native-search-usage;other-plugin/thing\n" +
		"DSHEVENTS\tTYPE\tcost-meter/native-search-usage\t1\t10\n" +
		"DSHEVENTS\tTYPE\tother-plugin/thing\t1\t2\n" +
		"DSHEVENTS\tWARN\tboom\tfile.jsonl.zstd\n" +
		"DSHEVENTS\tDONE\t1\t2\n"
	scan, errMsg := parseSessionEventScan(out)
	if errMsg != "" {
		t.Fatalf("unexpected err: %s", errMsg)
	}
	if len(scan.Stats) != 2 || scan.Stats[0].Type != "cost-meter/native-search-usage" || scan.Stats[0].Sessions != 1 || scan.Stats[0].Events != 10 {
		t.Fatalf("stats = %+v", scan.Stats)
	}
	if got := scan.BySession["scope/a/session.v3.jsonl.zstd"]; !reflect.DeepEqual(got, []string{"cost-meter/native-search-usage", "other-plugin/thing"}) {
		t.Fatalf("bySession = %v", got)
	}
	if len(scan.Warnings) != 1 {
		t.Fatalf("warnings = %v", scan.Warnings)
	}

	if _, errMsg := parseSessionEventScan("DSHEVENTS\tERR\t未找到 harness 事件白名单\n"); errMsg == "" {
		t.Fatal("expected error message from ERR line")
	}
}

func TestParseSessionEventRepair(t *testing.T) {
	files, events, skipped, failed, errMsg := parseSessionEventRepair("DSHEVENTS\tWARN\tf\tboom\nDSHEVENTS\tDONE\t3\t20\t1\t2\n")
	if errMsg != "" || files != 3 || events != 20 || skipped != 1 || failed != 2 {
		t.Fatalf("parsed = %d/%d/%d/%d err=%q", files, events, skipped, failed, errMsg)
	}
	if _, _, _, _, errMsg := parseSessionEventRepair("DSHEVENTS\tERR\tno node\n"); errMsg != "no node" {
		t.Fatalf("errMsg = %q", errMsg)
	}
	// 老版本脚本只输出两个字段：跳过/失败按 0 处理，不panic。
	if files, events, skipped, failed, _ := parseSessionEventRepair("DSHEVENTS\tDONE\t2\t7\n"); files != 2 || events != 7 || skipped != 0 || failed != 0 {
		t.Fatalf("short DONE line parsed = %d/%d/%d/%d", files, events, skipped, failed)
	}
}

func TestAtoiSafe(t *testing.T) {
	cases := map[string]int{"12": 12, "0": 0, " 7 ": 7, "x": 0, "-3": 0, "": 0, "1 2": 0}
	for in, want := range cases {
		if got := atoiSafe(in); got != want {
			t.Errorf("atoiSafe(%q) = %d, want %d", in, got, want)
		}
	}
}
