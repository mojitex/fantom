package optimizer

import (
	"fmt"
	"strings"
	"testing"
)

func TestRTKDetectsAndCompactsGitDiff(t *testing.T) {
	lines:=[]string{"diff --git a/main.go b/main.go","index abc..def 100644","--- a/main.go","+++ b/main.go","@@ -1,0 +1,180 @@"}
	for i:=0;i<180;i++ {lines=append(lines,fmt.Sprintf("+added source line %03d %s",i,strings.Repeat("x",32)))}
	input:=strings.Join(lines,"\n")
	if got:=detectRTKFilter(input);got!="git-diff" {t.Fatalf("detected %q, want git-diff",got)}
	output:=compactGitDiff(input)
	if len(output)>=len(input) {t.Fatalf("git diff did not shrink: before=%d after=%d",len(input),len(output))}
	if !strings.Contains(output,"main.go") || !strings.Contains(output,"+180 -0") || !strings.Contains(output,"lines truncated") {
		t.Fatalf("git diff summary lost important information: %s",output)
	}
}

func TestRTKCompactsGrepAndFindOutput(t *testing.T) {
	grepLines:=[]string{}
	for i:=1;i<=16;i++ {grepLines=append(grepLines,fmt.Sprintf("src/very/long/path/example.go:%d:   match result %s",i,strings.Repeat("details ",8)))}
	grepInput:=strings.Join(grepLines,"\n")
	if got:=detectRTKFilter(grepInput);got!="grep" {t.Fatalf("grep filter detection = %q",got)}
	grepOutput:=compactGrep(grepInput)
	if len(grepOutput)>=len(grepInput) || !strings.Contains(grepOutput,"16 matches in 1 files") || !strings.Contains(grepOutput,"+6 more") {
		t.Fatalf("grep output was not grouped/capped: %s",grepOutput)
	}

	findLines:=[]string{}
	for i:=0;i<30;i++ {findLines=append(findLines,fmt.Sprintf("./a/deep/project/directory/with/a/very/long/filename_%02d_generated_component.go",i))}
	findInput:=strings.Join(findLines,"\n")
	if got:=detectRTKFilter(findInput);got!="find" {t.Fatalf("find filter detection = %q",got)}
	findOutput:=compactFind(findInput)
	if len(findOutput)>=len(findInput) || !strings.Contains(findOutput,"30 files in 1 dirs") || !strings.Contains(findOutput,"+20 more") {
		t.Fatalf("find output was not compacted: %s",findOutput)
	}
}

func TestRTKCompactsGitStatusAndLSTable(t *testing.T) {
	status:=strings.Join([]string{
		"## feature/rtk",
		" M internal/server/server.go",
		"A  internal/optimizer/rtk_filters.go",
		"?? very/long/path/to/newly/generated/test-fixture.data",
		"UU go.mod",
	},"\n")
	if got:=detectRTKFilter(status);got!="git-status" {t.Fatalf("git status filter detection = %q",got)}
	statusOutput:=compactGitStatus(status)
	for _,want:=range []string{"* feature/rtk","+ Staged: 1 files","~ Modified: 1 files","? Untracked: 1 files","conflicts: 1 files"} {
		if !strings.Contains(statusOutput,want) {t.Errorf("git status output missing %q: %s",want,statusOutput)}
	}

	ls:=strings.Join([]string{
		"total 48",
		"drwxr-xr-x 2 user staff 64 Jan 1 12:00 .",
		"drwxr-xr-x 4 user staff 128 Jan 1 12:00 ..",
		"drwxr-xr-x 3 user staff 96 Jan 1 12:00 node_modules",
		"drwxr-xr-x 2 user staff 64 Jan 1 12:00 src",
		"-rw-r--r-- 1 user staff 2048 Jan 1 12:00 README.md",
		"-rw-r--r-- 1 user staff 4096 Jan 1 12:00 main.go",
	},"\n")
	if got:=detectRTKFilter(ls);got!="ls" {t.Fatalf("ls filter detection = %q",got)}
	lsOutput:=compactLS(ls)
	if !strings.Contains(lsOutput,"src/") || !strings.Contains(lsOutput,"README.md  2.0K") || strings.Contains(lsOutput,"node_modules") {
		t.Fatalf("ls compaction output unexpected: %s",lsOutput)
	}
}

func TestRTKCompactsGitLogAndBuildOutput(t *testing.T) {
	gitLog:=strings.Join([]string{
		"commit abcdef0123456789abcdef0123456789abcdef01",
		"Author: Dev <dev@example.com>",
		"Date:   Fri Nov 6 12:00:00 2026 +0330",
		"",
		"    Add structured output filtering",
		"",
		"    This long body is intentionally verbose and should be omitted by the compact log view.",
		"    More explanatory details that are not needed in the model context.",
		" 3 files changed, 120 insertions(+), 30 deletions(-)",
		"diff --git a/a.go b/a.go",
		"+ source line",
		"commit 1234567890abcdef1234567890abcdef12345678",
		"Author: Another Dev",
		"",
		"    Fix edge case",
		" 1 file changed, 2 insertions(+)",
	},"\n")
	if got:=detectRTKFilter(gitLog);got!="git-log" {t.Fatalf("git log detected as %q",got)}
	gitLogOutput:=compactGitLog(gitLog)
	if len(gitLogOutput)>=len(gitLog) ||
		!strings.Contains(gitLogOutput,"abcdef0123456789abcdef0123456789abcdef01") ||
		!strings.Contains(gitLogOutput,"Subject: Add structured output filtering") ||
		!strings.Contains(gitLogOutput,"3 files changed") ||
		!strings.Contains(gitLogOutput,"diff body omitted") ||
		strings.Contains(gitLogOutput,"long body is intentionally") {
		t.Fatalf("git log compacting lost metadata or retained body: %s",gitLogOutput)
	}

	build:=strings.Join([]string{
		"Compiling crate_a v1.0.0",
		"Compiling crate_b v1.0.0",
		"Downloading crate_c",
		"warning: unused variable",
		"  --> src/main.rs:10:5",
		"  |",
		"10 | let unused = 1;",
		"error[E0308]: mismatched types",
		"  --> src/lib.rs:20:3",
		"  | expected String, found integer",
		"Finished dev profile [unoptimized + debuginfo] target(s) in 3.2s",
	},"\n")
	if got:=detectRTKFilter(build);got!="build-output" {t.Fatalf("build output detected as %q",got)}
	buildOutput:=compactBuildOutput(build)
	for _,want:=range []string{"Compiled 2 packages","Downloaded 1 packages","warning: unused variable","error[E0308]: mismatched types","Finished dev profile"} {
		if !strings.Contains(buildOutput,want) {t.Errorf("build summary missing %q: %s",want,buildOutput)}
	}
	if len(buildOutput)>=len(build) {t.Fatalf("build output did not shrink: before=%d after=%d",len(build),len(buildOutput))}
}

func TestRTKHandlesWindowsGrepAndLongFormUntrackedGitStatus(t *testing.T) {
	windowsGrep:=strings.Join([]string{
		"C:\\workspace\\repo\\main.go:10:"+strings.Repeat("match context ",12),
		"C:\\workspace\\repo\\main.go:11:"+strings.Repeat("another context ",12),
		"C:\\workspace\\repo\\other.go:4:"+strings.Repeat("third context ",12),
		"C:\\workspace\\repo\\other.go:5:"+strings.Repeat("fourth context ",12),
		"C:\\workspace\\repo\\other.go:6:"+strings.Repeat("fifth context ",12),
	},"\n")
	if got:=detectRTKFilter(windowsGrep);got!="grep" {
		t.Fatalf("Windows grep format detected as %q",got)
	}
	grepOutput:=compactGrep(windowsGrep)
	if !strings.Contains(grepOutput,"5 matches in 2 files") || !strings.Contains(grepOutput,"C:\\workspace\\repo\\main.go") {
		t.Fatalf("Windows grep paths were not preserved: %s",grepOutput)
	}

	status:=strings.Join([]string{
		"On branch feature/new-files",
		"Untracked files:",
		"  (use \"git add <file>...\" to include in what will be committed)",
		"        docs/new-specification.md",
		"        generated/test-fixture.json",
	},"\n")
	if got:=detectRTKFilter(status);got!="git-status" {
		t.Fatalf("long-form status detected as %q",got)
	}
	statusOutput:=compactGitStatus(status)
	if !strings.Contains(statusOutput,"? Untracked: 2 files") || strings.Contains(statusOutput,"clean") {
		t.Fatalf("untracked files were lost or mislabeled as clean: %s",statusOutput)
	}
}

func TestRTKCompactsTreeSearchListAndLongFileReads(t *testing.T) {
	tree:=strings.Join([]string{"project","├── src","│  ├── main.go","│  └── util.go","└── tests","3 directories, 2 files",""},"\n")
	if got:=detectRTKFilter(tree);got!="tree" {t.Fatalf("tree filter detection = %q",got)}
	treeOutput:=compactTree(tree)
	if strings.Contains(treeOutput,"3 directories") {t.Fatal("tree summary line was not removed")}
	fileTree:="project\\n└── directory_file_notes.txt\\n3 directories, 2 files"
	if got:=compactTree(fileTree);!strings.Contains(got,"directory_file_notes.txt") {
		t.Fatalf("tree filter removed a path that merely contains summary words: %s",got)
	}

	search:=[]string{"Result of search in 'src' (total 24 files):"}
	for i:=0;i<24;i++ {search=append(search,fmt.Sprintf("- src/generated/deep/long/path/module_%02d_component_file.go",i))}
	searchInput:=strings.Join(search,"\n")
	if got:=detectRTKFilter(searchInput);got!="search-list" {t.Fatalf("search-list filter detection = %q",got)}
	searchOutput:=compactSearchList(searchInput)
	if len(searchOutput)>=len(searchInput) || !strings.Contains(searchOutput,"24 files in 1 dirs") || !strings.Contains(searchOutput,"+14 more") {
		t.Fatalf("search-list output was not compacted: %s",searchOutput)
	}

	numbered:=make([]string,rtkSmartMinLines+40)
	for i:=range numbered {numbered[i]=fmt.Sprintf("%4d| source line %d",i+1,i+1)}
	numberedInput:=strings.Join(numbered,"\n")
	if got:=detectRTKFilter(numberedInput);got!="read-numbered" {t.Fatalf("numbered-file filter detection = %q",got)}
	numberedOutput:=compactReadNumbered(numberedInput)
	if len(numberedOutput)>=len(numberedInput) || !strings.Contains(numberedOutput,"file continues") || !strings.Contains(numberedOutput,"source line 1") || !strings.Contains(numberedOutput,fmt.Sprintf("source line %d",len(numbered))) {
		t.Fatalf("numbered file output did not preserve head and tail: %s",numberedOutput)
	}
}

func TestRTKCompactionIsIntegratedAndNeverExpandsToolOutput(t *testing.T) {
	input:=strings.Join([]string{
		"src/very/long/path/to/generated/components/that/repeats/file.go:10:"+strings.Repeat("match context ",4),
		"src/very/long/path/to/generated/components/that/repeats/file.go:11:"+strings.Repeat("another context ",4),
		"src/very/long/path/to/generated/components/that/repeats/file.go:12:"+strings.Repeat("third context ",4),
		"src/very/long/path/to/generated/components/that/repeats/file.go:13:"+strings.Repeat("fourth context ",4),
		"src/very/long/path/to/generated/components/that/repeats/file.go:14:"+strings.Repeat("fifth context ",4),
	},"\n")
	request:=fmt.Sprintf("{\"messages\":[{\"role\":\"assistant\",\"tool_calls\":[{\"id\":\"call-1\",\"function\":{\"name\":\"bash\"}}]},{\"role\":\"tool\",\"tool_call_id\":\"call-1\",\"content\":%q}]}",input)
	optimized,stats:=Apply([]byte(request),"safe","off")
	if len(optimized)>=len(request) || stats.ToolResultsChanged!=1 {
		t.Fatalf("RTK filter was not applied in Apply(): before=%d after=%d stats=%#v",len(request),len(optimized),stats)
	}
	if !strings.Contains(string(optimized),"5 matches in 1 files") {
		t.Fatalf("integrated RTK result missing grep summary: %s",optimized)
	}

	unknown:=strings.Repeat("x",800)
	if got:=autoCompactRTK(unknown);got!=unknown {
		t.Fatalf("unrecognized short output should remain unchanged")
	}
}
