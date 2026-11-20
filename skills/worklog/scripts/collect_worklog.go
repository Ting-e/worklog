// Command collect_worklog read-only aggregates Git commits, local logs, and
// manual notes into a single JSON document.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	maxCommitsPerRepository = 200
	maxFilesPerCommit       = 50
	maxCommitBodyChars      = 2000
	defaultMaxLogChars      = 12000
)

var (
	localTimezone = time.FixedZone("Asia/Shanghai", 8*60*60)
	bearerPattern = regexp.MustCompile(
		`(?i)(["']?\bauthorization\b["']?\s*[:=]\s*["']?bearer\s+)[a-z0-9._~+/=-]+`,
	)
	secretPattern = regexp.MustCompile(
		`(?i)(["']?\b(?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|password|passwd|secret|authorization)\b["']?\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;&]+)`,
	)
	urlCredentialPattern = regexp.MustCompile(`(?i)(https?://)[^/\s:@]+:[^/\s@]+@`)
	privateKeyPattern    = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
	genericKeyPattern    = regexp.MustCompile(`(?i)\bsk-[a-z0-9_-]{16,}\b`)
	hashPattern          = regexp.MustCompile(`^[0-9a-fA-F]{40,64}$`)
)

type stringList []string

func (values *stringList) String() string {
	return strings.Join(*values, ",")
}

func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type options struct {
	period        string
	referenceDate string
	startDate     string
	endDate       string
	repositories  stringList
	logFiles      stringList
	notes         stringList
	author        string
	allAuthors    bool
	maxLogChars   int
}

type period struct {
	Type      string `json:"type"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	Start     string `json:"start"`
	End       string `json:"end"`
	Timezone  string `json:"timezone"`
}

type fileChange struct {
	Path       string `json:"path"`
	Insertions *int   `json:"insertions"`
	Deletions  *int   `json:"deletions"`
	Binary     bool   `json:"binary"`
}

type commitStats struct {
	FilesChanged      int          `json:"files_changed"`
	Insertions        int          `json:"insertions"`
	Deletions         int          `json:"deletions"`
	BinaryFiles       int          `json:"binary_files"`
	Files             []fileChange `json:"files"`
	FileListTruncated bool         `json:"file_list_truncated"`
}

type commit struct {
	Hash          string      `json:"hash"`
	ShortHash     string      `json:"short_hash"`
	Author        string      `json:"author"`
	CommittedAt   string      `json:"committed_at"`
	Subject       string      `json:"subject"`
	Body          string      `json:"body"`
	BodyTruncated bool        `json:"body_truncated"`
	Stats         commitStats `json:"stats"`
}

type repositoryTotals struct {
	FileChanges int `json:"file_changes"`
	Insertions  int `json:"insertions"`
	Deletions   int `json:"deletions"`
}

type repository struct {
	Path             string           `json:"path"`
	AuthorFilter     string           `json:"author_filter"`
	CommitCount      int              `json:"commit_count"`
	CommitsTruncated bool             `json:"commits_truncated"`
	Totals           repositoryTotals `json:"totals"`
	Commits          []commit         `json:"commits"`
}

type logEntry struct {
	Path         string `json:"path"`
	Content      string `json:"content"`
	ContentChars int    `json:"content_chars"`
	Truncated    bool   `json:"truncated"`
}

type collectionTotals struct {
	Repositories int `json:"repositories"`
	Commits      int `json:"commits"`
	FileChanges  int `json:"file_changes"`
	Insertions   int `json:"insertions"`
	Deletions    int `json:"deletions"`
	LogFiles     int `json:"log_files"`
	ManualNotes  int `json:"manual_notes"`
}

type collectionLimits struct {
	MaxCommitsPerRepository int `json:"max_commits_per_repository"`
	MaxFilesPerCommit       int `json:"max_files_per_commit"`
	MaxCommitBodyChars      int `json:"max_commit_body_chars"`
	MaxLogCharsPerFile      int `json:"max_log_chars_per_file"`
}

type collection struct {
	SchemaVersion    string           `json:"schema_version"`
	GeneratedAt      string           `json:"generated_at"`
	Period           period           `json:"period"`
	Repositories     []repository     `json:"repositories"`
	Logs             []logEntry       `json:"logs"`
	ManualNotes      []string         `json:"manual_notes"`
	Totals           collectionTotals `json:"totals"`
	CollectionLimits collectionLimits `json:"collection_limits"`
}

type collectionError struct {
	message string
}

func (err *collectionError) Error() string {
	return err.message
}

func newCollectionError(format string, arguments ...any) error {
	return &collectionError{message: fmt.Sprintf(format, arguments...)}
}

func parseDate(value, flagName string) (time.Time, error) {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, newCollectionError("%s must use YYYY-MM-DD format: %q", flagName, value)
	}
	return parsed, nil
}

func parseOptions(arguments []string) (options, error) {
	var opts options
	flags := flag.NewFlagSet("collect_worklog", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&opts.period, "period", "daily", "Collection period: daily, weekly, or custom.")
	flags.StringVar(&opts.referenceDate, "reference-date", "", "Reference date for daily or weekly periods.")
	flags.StringVar(&opts.startDate, "start", "", "Custom period start date.")
	flags.StringVar(&opts.endDate, "end", "", "Custom period end date.")
	flags.Var(&opts.repositories, "repo", "Git repository path. Repeat for multiple repositories.")
	flags.Var(&opts.logFiles, "log-file", "UTF-8 local log file. Repeat for multiple files.")
	flags.Var(&opts.notes, "note", "Manual work note. Repeat for multiple notes.")
	flags.StringVar(&opts.author, "author", "", "Git author filter applied to every repository.")
	flags.BoolVar(&opts.allAuthors, "all-authors", false, "Disable author filtering.")
	flags.IntVar(&opts.maxLogChars, "max-log-chars", defaultMaxLogChars, "Maximum characters sampled per log file.")
	if err := flags.Parse(arguments); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, newCollectionError("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if opts.period != "daily" && opts.period != "weekly" && opts.period != "custom" {
		return opts, newCollectionError("--period must be daily, weekly, or custom")
	}
	if opts.author != "" && opts.allAuthors {
		return opts, newCollectionError("--author and --all-authors are mutually exclusive")
	}
	hasNote := false
	for _, note := range opts.notes {
		if strings.TrimSpace(note) != "" {
			hasNote = true
			break
		}
	}
	if len(opts.repositories) == 0 && len(opts.logFiles) == 0 && !hasNote {
		return opts, newCollectionError("provide at least one --repo, --log-file, or non-empty --note")
	}
	if opts.maxLogChars < 100 {
		return opts, newCollectionError("--max-log-chars must be at least 100")
	}
	if opts.period == "custom" {
		if opts.startDate == "" || opts.endDate == "" {
			return opts, newCollectionError("custom periods require both --start and --end")
		}
		start, err := parseDate(opts.startDate, "--start")
		if err != nil {
			return opts, err
		}
		end, err := parseDate(opts.endDate, "--end")
		if err != nil {
			return opts, err
		}
		if start.After(end) {
			return opts, newCollectionError("--start cannot be later than --end")
		}
	} else if opts.startDate != "" || opts.endDate != "" {
		return opts, newCollectionError("--start and --end are only valid with --period custom")
	}
	if opts.referenceDate != "" {
		if _, err := parseDate(opts.referenceDate, "--reference-date"); err != nil {
			return opts, err
		}
	}
	return opts, nil
}

func resolvePeriod(opts options) (period, error) {
	reference := time.Now().In(localTimezone)
	if opts.referenceDate != "" {
		parsed, err := parseDate(opts.referenceDate, "--reference-date")
		if err != nil {
			return period{}, err
		}
		reference = parsed
	}

	startDate := reference
	endDate := reference
	switch opts.period {
	case "weekly":
		weekday := int(reference.Weekday())
		daysSinceMonday := (weekday + 6) % 7
		startDate = reference.AddDate(0, 0, -daysSinceMonday)
		endDate = startDate.AddDate(0, 0, 6)
	case "custom":
		var err error
		startDate, err = parseDate(opts.startDate, "--start")
		if err != nil {
			return period{}, err
		}
		endDate, err = parseDate(opts.endDate, "--end")
		if err != nil {
			return period{}, err
		}
	}

	start := time.Date(startDate.Year(), startDate.Month(), startDate.Day(), 0, 0, 0, 0, localTimezone)
	end := time.Date(endDate.Year(), endDate.Month(), endDate.Day(), 23, 59, 59, 999999000, localTimezone)
	return period{
		Type:      opts.period,
		StartDate: startDate.Format("2006-01-02"),
		EndDate:   endDate.Format("2006-01-02"),
		Start:     start.Format(time.RFC3339Nano),
		End:       end.Format(time.RFC3339Nano),
		Timezone:  "Asia/Shanghai (UTC+08:00)",
	}, nil
}

func redact(text string) string {
	text = privateKeyPattern.ReplaceAllString(text, "[REDACTED]")
	text = bearerPattern.ReplaceAllString(text, "${1}[REDACTED]")
	text = secretPattern.ReplaceAllString(text, "${1}[REDACTED]")
	text = urlCredentialPattern.ReplaceAllString(text, "${1}[REDACTED]@")
	return genericKeyPattern.ReplaceAllString(text, "[REDACTED]")
}

func runGit(repo string, allowedReturnCodes []int, arguments ...string) (string, error) {
	commandArguments := []string{"-C", repo}
	commandArguments = append(commandArguments, arguments...)
	command := exec.Command("git", commandArguments...)
	output, err := command.Output()
	if err == nil {
		return strings.ToValidUTF8(string(output), "�"), nil
	}

	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		returnCode := exitError.ExitCode()
		for _, allowed := range allowedReturnCodes {
			if returnCode == allowed {
				return strings.ToValidUTF8(string(output), "�"), nil
			}
		}
		detail := strings.TrimSpace(strings.ToValidUTF8(string(exitError.Stderr), "�"))
		if detail == "" {
			detail = fmt.Sprintf("exit code %d", returnCode)
		}
		return "", newCollectionError("Git command failed for %s: %s", repo, detail)
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "", newCollectionError("git command not found; install Git and add it to PATH")
	}
	return "", newCollectionError("failed to run Git for %s: %v", repo, err)
}

func resolveRepository(pathValue string) (string, error) {
	root, err := runGit(pathValue, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return "", newCollectionError("cannot identify Git repository: %s", pathValue)
	}
	resolved, err := filepath.Abs(filepath.FromSlash(root))
	if err != nil {
		return "", err
	}
	if evaluated, evalErr := filepath.EvalSymlinks(resolved); evalErr == nil {
		resolved = evaluated
	}
	return filepath.Clean(resolved), nil
}

func defaultAuthor(repo string) (string, error) {
	for _, key := range []string{"user.name", "user.email"} {
		value, err := runGit(repo, []int{1}, "config", "--get", key)
		if err != nil {
			return "", err
		}
		if value = strings.TrimSpace(value); value != "" {
			return value, nil
		}
	}
	return "", newCollectionError("repository %s has no user.name or user.email; use --author or --all-authors", repo)
}

func parseCommitFields(rawOutput string) ([]commit, error) {
	fields := strings.Split(rawOutput, "\x00")
	commits := make([]commit, 0)
	for index := 0; index < len(fields)-1; index += 6 {
		if index+6 > len(fields) {
			break
		}
		group := fields[index : index+6]
		fullHash := strings.TrimLeft(group[0], "\r\n")
		if !hashPattern.MatchString(fullHash) {
			if strings.TrimSpace(fullHash) == "" {
				continue
			}
			return nil, newCollectionError("cannot parse Git commit output; check the Git output format")
		}
		body := redact(strings.TrimSpace(group[5]))
		bodyRunes := []rune(body)
		bodyTruncated := len(bodyRunes) > maxCommitBodyChars
		if bodyTruncated {
			body = string(bodyRunes[:maxCommitBodyChars])
		}
		commits = append(commits, commit{
			Hash:          fullHash,
			ShortHash:     group[1],
			Author:        redact(group[2]),
			CommittedAt:   group[3],
			Subject:       redact(group[4]),
			Body:          body,
			BodyTruncated: bodyTruncated,
		})
	}
	return commits, nil
}

func collectCommitStats(repo, commitHash string) (commitStats, error) {
	output, err := runGit(repo, nil,
		"show",
		"--numstat",
		"--format=",
		"--no-renames",
		"--no-ext-diff",
		"--no-textconv",
		commitHash,
	)
	if err != nil {
		return commitStats{}, err
	}
	stats := commitStats{Files: make([]fileChange, 0)}
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
		if len(parts) != 3 {
			continue
		}
		stats.FilesChanged++
		isBinary := parts[0] == "-" || parts[1] == "-"
		var insertions, deletions *int
		if isBinary {
			stats.BinaryFiles++
		} else {
			added, addErr := strconv.Atoi(parts[0])
			deleted, deleteErr := strconv.Atoi(parts[1])
			if addErr != nil || deleteErr != nil {
				return commitStats{}, newCollectionError("cannot parse Git numstat output for commit %s", commitHash)
			}
			stats.Insertions += added
			stats.Deletions += deleted
			insertions = &added
			deletions = &deleted
		}
		if len(stats.Files) < maxFilesPerCommit {
			stats.Files = append(stats.Files, fileChange{
				Path:       parts[2],
				Insertions: insertions,
				Deletions:  deletions,
				Binary:     isBinary,
			})
		}
	}
	stats.FileListTruncated = stats.FilesChanged > maxFilesPerCommit
	return stats, nil
}

func collectRepository(repo string, bounds period, author string) (repository, error) {
	arguments := []string{
		"log",
		"--all",
		"--no-merges",
		"--since=" + bounds.Start,
		"--until=" + bounds.End,
		fmt.Sprintf("--max-count=%d", maxCommitsPerRepository+1),
		"--format=%H%x00%h%x00%an%x00%cI%x00%s%x00%b%x00",
	}
	if author != "" {
		arguments = append(arguments, "--author="+author)
	}
	output, err := runGit(repo, nil, arguments...)
	if err != nil {
		return repository{}, err
	}
	commits, err := parseCommitFields(output)
	if err != nil {
		return repository{}, err
	}
	commitsTruncated := len(commits) > maxCommitsPerRepository
	if commitsTruncated {
		commits = commits[:maxCommitsPerRepository]
	}

	result := repository{
		Path:             repo,
		AuthorFilter:     author,
		CommitCount:      len(commits),
		CommitsTruncated: commitsTruncated,
		Commits:          commits,
	}
	if result.AuthorFilter == "" {
		result.AuthorFilter = "*"
	}
	for index := range result.Commits {
		stats, statsErr := collectCommitStats(repo, result.Commits[index].Hash)
		if statsErr != nil {
			return repository{}, statsErr
		}
		result.Commits[index].Stats = stats
		result.Totals.FileChanges += stats.FilesChanged
		result.Totals.Insertions += stats.Insertions
		result.Totals.Deletions += stats.Deletions
	}
	return result, nil
}

func readLogFile(pathValue string, maxChars int) (logEntry, error) {
	path, err := filepath.Abs(pathValue)
	if err != nil {
		return logEntry{}, err
	}
	if evaluated, evalErr := filepath.EvalSymlinks(path); evalErr == nil {
		path = evaluated
	}
	info, err := os.Stat(path)
	if err != nil {
		return logEntry{}, err
	}
	if !info.Mode().IsRegular() {
		return logEntry{}, newCollectionError("log path is not a regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return logEntry{}, err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	runes := make([]rune, 0, maxChars+1)
	for len(runes) < maxChars+1 {
		character, _, readErr := reader.ReadRune()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return logEntry{}, readErr
		}
		runes = append(runes, character)
	}
	if len(runes) > 0 && runes[0] == '\ufeff' {
		runes = runes[1:]
	}
	truncated := len(runes) > maxChars
	if truncated {
		runes = runes[:maxChars]
	}
	content := string(runes)
	return logEntry{
		Path:         filepath.Clean(path),
		Content:      redact(content),
		ContentChars: len(runes),
		Truncated:    truncated,
	}, nil
}

func normalizedPathKey(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func collect(opts options) (collection, error) {
	bounds, err := resolvePeriod(opts)
	if err != nil {
		return collection{}, err
	}
	result := collection{
		SchemaVersion: "1.0",
		GeneratedAt:   time.Now().In(localTimezone).Format(time.RFC3339Nano),
		Period:        bounds,
		Repositories:  make([]repository, 0),
		Logs:          make([]logEntry, 0),
		ManualNotes:   make([]string, 0),
		CollectionLimits: collectionLimits{
			MaxCommitsPerRepository: maxCommitsPerRepository,
			MaxFilesPerCommit:       maxFilesPerCommit,
			MaxCommitBodyChars:      maxCommitBodyChars,
			MaxLogCharsPerFile:      opts.maxLogChars,
		},
	}

	seenRepositories := make(map[string]struct{})
	for _, repoValue := range opts.repositories {
		repo, resolveErr := resolveRepository(repoValue)
		if resolveErr != nil {
			return collection{}, resolveErr
		}
		key := normalizedPathKey(repo)
		if _, seen := seenRepositories[key]; seen {
			continue
		}
		seenRepositories[key] = struct{}{}
		author := opts.author
		if !opts.allAuthors && author == "" {
			author, err = defaultAuthor(repo)
			if err != nil {
				return collection{}, err
			}
		}
		if opts.allAuthors {
			author = ""
		}
		collected, collectErr := collectRepository(repo, bounds, author)
		if collectErr != nil {
			return collection{}, collectErr
		}
		result.Repositories = append(result.Repositories, collected)
	}

	seenLogs := make(map[string]struct{})
	for _, logValue := range opts.logFiles {
		entry, readErr := readLogFile(logValue, opts.maxLogChars)
		if readErr != nil {
			return collection{}, readErr
		}
		key := normalizedPathKey(entry.Path)
		if _, seen := seenLogs[key]; seen {
			continue
		}
		seenLogs[key] = struct{}{}
		result.Logs = append(result.Logs, entry)
	}

	for _, note := range opts.notes {
		if note = strings.TrimSpace(note); note != "" {
			result.ManualNotes = append(result.ManualNotes, redact(note))
		}
	}

	result.Totals.Repositories = len(result.Repositories)
	result.Totals.LogFiles = len(result.Logs)
	result.Totals.ManualNotes = len(result.ManualNotes)
	for _, repo := range result.Repositories {
		result.Totals.Commits += repo.CommitCount
		result.Totals.FileChanges += repo.Totals.FileChanges
		result.Totals.Insertions += repo.Totals.Insertions
		result.Totals.Deletions += repo.Totals.Deletions
	}
	return result, nil
}

func run(arguments []string) error {
	opts, err := parseOptions(arguments)
	if err != nil {
		return err
	}
	result, err := collect(opts)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "collection failed: %v\n", err)
		os.Exit(2)
	}
}
