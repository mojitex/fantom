package optimizer

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	rtkDetectWindow     = 1024
	rtkMaxGrepPerFile   = 10
	rtkMaxFindPerDir    = 10
	rtkMaxFindDirs      = 20
	rtkMaxStatusFiles   = 10
	rtkMaxTreeLines     = 200
	rtkMaxDiffLines     = 500
	rtkMaxDiffHunkLines = 100
	rtkSmartHeadLines   = 120
	rtkSmartTailLines   = 60
	rtkSmartMinLines    = 250
	rtkDedupMaxLines    = 2000
)

var (
	rtkDiffHeaderRE = regexp.MustCompile("^diff --git ")
	rtkDiffHunkRE   = regexp.MustCompile("^@@ ")
	rtkStatusRE     = regexp.MustCompile("^(On branch |nothing to commit|Changes (not |to be )|Untracked files:)")
	rtkPorcelainRE  = regexp.MustCompile("^[ MADRCU?!][ MADRCU?!] \\S")
	rtkLSRowRE      = regexp.MustCompile("^[-dlbcps][rwx-]{9}")
	rtkLSTotalRE    = regexp.MustCompile("^total \\d+$")
	rtkTreeGlyphRE  = regexp.MustCompile("[├└]──|│  ")
	rtkSearchRE     = regexp.MustCompile("^Result of search in '[^']*' \\(total \\d+ files?\\):")
	rtkNumberedRE   = regexp.MustCompile("^\\s*\\d+\\|")
	rtkGrepLineRE   = regexp.MustCompile("^(.+):([0-9]+):(.*)$")
	rtkDateRE       = regexp.MustCompile("\\s+(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\\s+\\d{1,2}\\s+(\\d{4}|\\d{2}:\\d{2})\\s+")
	rtkWindowsPathRE = regexp.MustCompile("^[A-Za-z]:[\\\\/]")
)

type rtkLineMatch struct {
	lineNumber string
	content    string
}

func detectRTKFilter(text string) string {
	head := text
	if len(head) > rtkDetectWindow {
		head = head[:rtkDetectWindow]
	}
	switch {
	case rtkDiffHeaderRE.MatchString(head) || rtkDiffHunkRE.MatchString(head):
		return "git-diff"
	case rtkStatusRE.MatchString(head) || mostlyGitPorcelain(head):
		return "git-status"
	}
	lines := strings.Split(head, "\n")
	nonEmpty := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			nonEmpty = append(nonEmpty, line)
		}
	}
	for i, line := range nonEmpty {
		if i >= 5 {
			break
		}
		if _, _, ok := parseGrepLine(line); ok {
			return "grep"
		}
	}
	if len(nonEmpty) >= 3 {
		allPaths := true
		for _, line := range nonEmpty {
			if !isPathLikeRTK(line) {
				allPaths = false
				break
			}
		}
		if allPaths {
			return "find"
		}
	}
	switch {
	case rtkTreeGlyphRE.MatchString(head):
		return "tree"
	case rtkLSTotalRE.MatchString(head) || countRE(rtkLSRowRE, head) >= 3:
		return "ls"
	case rtkSearchRE.MatchString(head):
		return "search-list"
	}
	if len(lines) >= rtkSmartMinLines && isMostlyNumbered(lines) {
		return "read-numbered"
	}
	if len(nonEmpty) >= 5 {
		return "dedup-log"
	}
	if len(lines) >= rtkSmartMinLines {
		return "smart-truncate"
	}
	return ""
}

func autoCompactRTK(text string) string {
	if len(text)<500 || len(text)>10<<20 { return text }
	switch detectRTKFilter(text) {
	case "git-diff":
		return compactGitDiff(text)
	case "git-status":
		return compactGitStatus(text)
	case "grep":
		return compactGrep(text)
	case "find":
		return compactFind(text)
	case "tree":
		return compactTree(text)
	case "ls":
		return compactLS(text)
	case "search-list":
		return compactSearchList(text)
	case "read-numbered":
		return compactReadNumbered(text)
	case "smart-truncate":
		return smartTruncate(text, rtkSmartHeadLines, rtkSmartTailLines)
	case "dedup-log":
		return dedupLog(text)
	default:
		return text
	}
}

func countRE(re *regexp.Regexp, value string) int {
	return len(re.FindAllStringIndex(value, -1))
}

func parseGrepLine(line string) (rtkLineMatch, string, bool) {
	match:=rtkGrepLineRE.FindStringSubmatch(line)
	if len(match)!=4 || match[1]=="" { return rtkLineMatch{},"",false }
	return rtkLineMatch{lineNumber:match[2],content:match[3]},match[1],true
}

func mostlyGitPorcelain(text string) bool {
	total, hits := 0, 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		total++
		if rtkPorcelainRE.MatchString(line) {
			hits++
		}
	}
	return total >= 3 && float64(hits)/float64(total) >= 0.6
}

func isPathLikeRTK(line string) bool {
	value := strings.TrimSpace(line)
	if value == "" {
		return false
	}
	if rtkWindowsPathRE.MatchString(value) {
		return true
	}
	if strings.Contains(value, ":") {
		return false
	}
	return strings.HasPrefix(value, ".") || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "/\\")
}

func compactGitDiff(input string) string {
	lines := strings.Split(input, "\n")
	out := make([]string, 0, len(lines))
	currentFile := ""
	added, removed := 0, 0
	inHunk, truncated := false, false
	shown, skipped := 0, 0
	flushSkipped := func() {
		if skipped > 0 {
			out = append(out, fmt.Sprintf("  ... (%d lines truncated)", skipped))
			truncated = true
			skipped = 0
		}
	}
	flushFile := func() {
		if currentFile != "" && (added > 0 || removed > 0) {
			out = append(out, fmt.Sprintf("  +%d -%d", added, removed))
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			flushSkipped()
			flushFile()
			parts := strings.SplitN(line, " b/", 2)
			currentFile = "unknown"
			if len(parts) == 2 && parts[1] != "" {
				currentFile = parts[1]
			}
			out = append(out, "\n"+currentFile)
			added, removed = 0, 0
			inHunk, shown = false, 0
		} else if strings.HasPrefix(line, "@@") {
			flushSkipped()
			inHunk, shown = true, 0
			out = append(out, "  "+line)
		} else if inHunk {
			isAdd := strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++")
			isRemove := strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---")
			isContext := !strings.HasPrefix(line, "\\")
			if isAdd {
				added++
			}
			if isRemove {
				removed++
			}
			if isAdd || isRemove {
				if shown < rtkMaxDiffHunkLines {
					out = append(out, "  "+line)
					shown++
				} else {
					skipped++
				}
			} else if isContext && shown > 0 && shown < rtkMaxDiffHunkLines {
				out = append(out, "  "+line)
				shown++
			}
		}
		if len(out) >= rtkMaxDiffLines {
			out = append(out, "\n... (more changes truncated)")
			truncated = true
			break
		}
	}
	flushSkipped()
	flushFile()
	if truncated {
		out = append(out, "[full diff: run git diff without compact filtering]")
	}
	return strings.Join(out, "\n")
}

func compactGitStatus(input string) string {
	lines:=strings.Split(input,"\n")
	branch:=""
	stagedFiles,modifiedFiles,untrackedFiles:=[]string{},[]string{},[]string{}
	staged,modified,untracked,conflicts:=0,0,0,0
	section:=""
	longBranchRE:=regexp.MustCompile("^On branch (\\S+)")
	longFileRE:=regexp.MustCompile("^\\s*(modified|new file|deleted|renamed|both modified):\\s+(.+)$")
	for _,raw:=range lines {
		line:=strings.TrimRight(raw,"\\r")
		trimmed:=strings.TrimSpace(line)
		if trimmed=="" {continue}
		if match:=longBranchRE.FindStringSubmatch(line);len(match)==2 {branch=match[1];section="";continue}
		if strings.HasPrefix(line,"##") {branch=strings.TrimSpace(strings.TrimPrefix(line,"##"));section="";continue}
		lower:=strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower,"changes to be committed:"):
			section="staged";continue
		case strings.HasPrefix(lower,"changes not staged for commit:"):
			section="modified";continue
		case strings.HasPrefix(lower,"untracked files:"):
			section="untracked";continue
		case strings.HasPrefix(lower,"unmerged paths:"):
			section="conflicts";continue
		case strings.HasPrefix(lower,"nothing to commit"):
			section="";continue
		}
		if len(line)>=3 && rtkPorcelainRE.MatchString(line) {
			x,y:=line[0],line[1]
			file:=strings.TrimSpace(line[3:])
			if x=='?' && y=='?' {untracked++;untrackedFiles=append(untrackedFiles,file);continue}
			if strings.ContainsRune("MADRC",rune(x)) {staged++;stagedFiles=append(stagedFiles,file)}
			if x=='U' || y=='U' {conflicts++}
			if y=='M' || y=='D' {modified++;modifiedFiles=append(modifiedFiles,file)}
			continue
		}
		if match:=longFileRE.FindStringSubmatch(line);len(match)==3 {
			switch match[1] {
			case "both modified":conflicts++
			case "modified","deleted":modified++;modifiedFiles=append(modifiedFiles,match[2])
			case "new file","renamed":staged++;stagedFiles=append(stagedFiles,match[2])
			}
			continue
		}
		if section!="" && strings.HasPrefix(line," ") && !strings.HasPrefix(trimmed,"(use ") {
			file:=trimmed
			switch section {
			case "staged":staged++;stagedFiles=append(stagedFiles,file)
			case "modified":modified++;modifiedFiles=append(modifiedFiles,file)
			case "untracked":untracked++;untrackedFiles=append(untrackedFiles,file)
			case "conflicts":conflicts++
			}
		}
	}
	out:=[]string{}
	if branch!="" {out=append(out,"* "+branch)}
	appendFiles:=func(label string,count int,files []string,limit int){
		if count==0{return}
		out=append(out,fmt.Sprintf("%s: %d files",label,count))
		for _,file:=range files[:minRTK(len(files),limit)] {out=append(out,"   "+file)}
		if len(files)>limit {out=append(out,fmt.Sprintf("   ... +%d more",len(files)-limit))}
	}
	appendFiles("+ Staged",staged,stagedFiles,rtkMaxStatusFiles)
	appendFiles("~ Modified",modified,modifiedFiles,rtkMaxStatusFiles)
	appendFiles("? Untracked",untracked,untrackedFiles,rtkMaxStatusFiles)
	if conflicts>0 {out=append(out,fmt.Sprintf("conflicts: %d files",conflicts))}
	if staged+modified+untracked+conflicts==0 {
		if strings.Contains(strings.ToLower(input),"nothing to commit") {out=append(out,"clean — nothing to commit")} else {return input}
	}
	return strings.Join(out,"\n")
}

func compactGrep(input string) string {
	byFile := map[string][]rtkLineMatch{}
	total := 0
	for _, line := range strings.Split(input, "\n") {
		match, file, ok := parseGrepLine(line)
		if !ok {
			continue
		}
		total++
		byFile[file] = append(byFile[file], match)
	}
	if total == 0 {
		return input
	}
	files := make([]string, 0, len(byFile))
	for file := range byFile {
		files = append(files, file)
	}
	sort.Strings(files)
	out := []string{fmt.Sprintf("%d matches in %d files:", total, len(files)), ""}
	for _, file := range files {
		matches := byFile[file]
		out = append(out, fmt.Sprintf("[file] %s (%d):", file, len(matches)))
		for _, match := range matches[:minRTK(len(matches), rtkMaxGrepPerFile)] {
			out = append(out, fmt.Sprintf("  %4s: %s", match.lineNumber, strings.TrimSpace(match.content)))
		}
		if len(matches) > rtkMaxGrepPerFile {
			out = append(out, fmt.Sprintf("  +%d more", len(matches)-rtkMaxGrepPerFile))
		}
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

func compactFind(input string) string {
	lines := []string{}
	for _, line := range strings.Split(input, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	if len(lines) == 0 {
		return input
	}
	byDir := map[string][]string{}
	for _, file := range lines {
		dir, base := path.Split(strings.ReplaceAll(file, "\\", "/"))
		dir = strings.TrimSuffix(dir, "/")
		if dir == "" {
			dir = "."
		}
		byDir[dir] = append(byDir[dir], base)
	}
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	out := []string{fmt.Sprintf("%d files in %d dirs:", len(lines), len(dirs)), ""}
	for _, dir := range dirs[:minRTK(len(dirs), rtkMaxFindDirs)] {
		files := byDir[dir]
		out = append(out, fmt.Sprintf("%s/ (%d)", dir, len(files)))
		for _, file := range files[:minRTK(len(files), rtkMaxFindPerDir)] {
			out = append(out, "  "+file)
		}
		if len(files) > rtkMaxFindPerDir {
			out = append(out, fmt.Sprintf("  +%d more", len(files)-rtkMaxFindPerDir))
		}
	}
	if len(dirs) > rtkMaxFindDirs {
		out = append(out, fmt.Sprintf("+%d more directories", len(dirs)-rtkMaxFindDirs))
	}
	return strings.Join(out, "\n")
}

func compactLS(input string) string {
	dirs := []string{}
	files := [][2]string{}
	extensions := map[string]int{}
	for _, line := range strings.Split(input, "\n") {
		if line == "" || rtkLSTotalRE.MatchString(line) {
			continue
		}
		date := rtkDateRE.FindStringIndex(line)
		if date == nil {
			continue
		}
		name := line[date[1]:]
		before := strings.Fields(line[:date[0]])
		if len(before) < 4 || name == "." || name == ".." || noiseEntry(name) {
			continue
		}
		kind := before[0][0]
		size := int64(0)
		for i := len(before)-1; i >= 0; i-- {
			if n, err := strconv.ParseInt(before[i], 10, 64); err == nil {
				size = n
				break
			}
		}
		if kind == 'd' {
			dirs = append(dirs, name)
		} else if kind == '-' || kind == 'l' {
			files = append(files, [2]string{name, humanSizeRTK(size)})
			ext := path.Ext(name)
			if ext == "" {
				ext = "no ext"
			}
			extensions[ext]++
		}
	}
	if len(dirs) == 0 && len(files) == 0 {
		return input
	}
	out := []string{}
	for _, dir := range dirs {
		out = append(out, dir+"/")
	}
	for _, file := range files {
		out = append(out, file[0]+"  "+file[1])
	}
	summary := fmt.Sprintf("Summary: %d files, %d dirs", len(files), len(dirs))
	type extensionCount struct {
		name string
		count int
	}
	ranked := make([]extensionCount, 0, len(extensions))
	for name, count := range extensions {
		ranked = append(ranked, extensionCount{name, count})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].count == ranked[j].count {
			return ranked[i].name < ranked[j].name
		}
		return ranked[i].count > ranked[j].count
	})
	if len(ranked) > 0 {
		n := minRTK(5, len(ranked))
		parts := []string{}
		for _, item := range ranked[:n] {
			parts = append(parts, fmt.Sprintf("%d %s", item.count, item.name))
		}
		if len(ranked) > n {
			parts = append(parts, fmt.Sprintf("+%d more", len(ranked)-n))
		}
		summary += " (" + strings.Join(parts, ", ") + ")"
	}
	out = append(out, "", summary)
	return strings.Join(out, "\n")
}

func humanSizeRTK(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%dB", size)
	}
}

func noiseEntry(name string) bool {
	switch strings.TrimSuffix(name, "/") {
	case "node_modules", ".git", "target", "__pycache__", ".next", "dist", "build", ".cache", ".turbo",
		".vercel", ".pytest_cache", ".mypy_cache", ".tox", ".venv", "venv", "env", "coverage",
		".nyc_output", ".DS_Store", "Thumbs.db", ".idea", ".vscode", ".vs", ".eggs":
		return true
	}
	return strings.HasSuffix(name, ".egg-info")
}

func compactTree(input string) string {
	lines := strings.Split(input, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "director") && strings.Contains(lower, "file") {
			continue
		}
		if strings.TrimSpace(line) == "" && len(out) == 0 {
			continue
		}
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	if len(out) > rtkMaxTreeLines {
		cut := len(out)-rtkMaxTreeLines
		out = append(out[:rtkMaxTreeLines], fmt.Sprintf("... +%d more lines", cut))
	}
	return strings.Join(out, "\n")
}

func compactSearchList(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) == 0 || !rtkSearchRE.MatchString(lines[0]) {
		return input
	}
	paths := []string{}
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") {
			paths = append(paths, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
		}
	}
	if len(paths) == 0 {
		return input
	}
	byDir := map[string][]string{}
	for _, value := range paths {
		dir, base := path.Split(strings.ReplaceAll(value, "\\", "/"))
		dir = strings.TrimSuffix(dir, "/")
		if dir == "" {
			dir = "."
		}
		byDir[dir] = append(byDir[dir], base)
	}
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	out := []string{lines[0], fmt.Sprintf("%d files in %d dirs:", len(paths), len(dirs)), ""}
	for _, dir := range dirs[:minRTK(len(dirs), rtkMaxFindDirs)] {
		files := byDir[dir]
		out = append(out, fmt.Sprintf("%s/ (%d):", dir, len(files)))
		for _, file := range files[:minRTK(len(files), rtkMaxFindPerDir)] {
			out = append(out, "  "+file)
		}
		if len(files) > rtkMaxFindPerDir {
			out = append(out, fmt.Sprintf("  +%d more", len(files)-rtkMaxFindPerDir))
		}
		out = append(out, "")
	}
	if len(dirs) > rtkMaxFindDirs {
		out = append(out, fmt.Sprintf("+%d more directories", len(dirs)-rtkMaxFindDirs))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func isMostlyNumbered(lines []string) bool {
	hits, nonEmpty := 0, 0
	for _, line := range lines[:minRTK(len(lines), 100)] {
		if line == "" {
			continue
		}
		nonEmpty++
		if rtkNumberedRE.MatchString(line) {
			hits++
		}
	}
	return nonEmpty >= 5 && float64(hits)/float64(nonEmpty) >= 0.7
}

func compactReadNumbered(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) < rtkSmartMinLines || len(lines) <= rtkSmartHeadLines+rtkSmartTailLines {
		return input
	}
	cut := len(lines)-rtkSmartHeadLines-rtkSmartTailLines
	out := append([]string{}, lines[:rtkSmartHeadLines]...)
	out = append(out, fmt.Sprintf("... +%d lines truncated (file continues)", cut))
	out = append(out, lines[len(lines)-rtkSmartTailLines:]...)
	return strings.Join(out, "\n")
}

func smartTruncate(input string, headCount, tailCount int) string {
	lines := strings.Split(input, "\n")
	if len(lines) < rtkSmartMinLines || len(lines) <= headCount+tailCount {
		return input
	}
	cut := len(lines)-headCount-tailCount
	out := append([]string{}, lines[:headCount]...)
	out = append(out, fmt.Sprintf("... +%d lines truncated", cut))
	out = append(out, lines[len(lines)-tailCount:]...)
	return strings.Join(out, "\n")
}

func dedupLog(input string) string {
	lines := strings.Split(input, "\n")
	out := make([]string, 0, len(lines))
	previous := ""
	run := 0
	blankAdded := false
	flush := func() {
		if run > 1 {
			out = append(out, fmt.Sprintf("  ... (%d duplicate lines)", run-1))
		}
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if !blankAdded {
				out = append(out, line)
			}
			blankAdded = true
			flush()
			previous, run = "", 0
			continue
		}
		blankAdded = false
		if run > 0 && line == previous {
			run++
			continue
		}
		flush()
		out = append(out, line)
		previous, run = line, 1
		if len(out) >= rtkDedupMaxLines {
			out = append(out, fmt.Sprintf("... (truncated at %d lines)", rtkDedupMaxLines))
			return strings.Join(out, "\n")
		}
	}
	flush()
	return strings.Join(out, "\n")
}

func minRTK(a, b int) int {
	if a < b { return a }
	return b
}
