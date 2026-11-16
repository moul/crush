package trust

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// root makes a directory of the test's own and returns its real path, so
// that paths compare equal on systems whose temporary directory is reached
// through a link.
func root(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// mkdir makes a directory and its parents, and returns its path.
func mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	dir := filepath.Join(parts...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// write writes a file, making the directories above it.
func write(t *testing.T, path, text string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// symlink makes link point at target.
func symlink(t *testing.T, target, link string) {
	t.Helper()
	mkdir(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// resolve runs Resolve and fails the test on an error.
func resolve(t *testing.T, start, home string) Folder {
	t.Helper()
	f, err := Resolve(start, home)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", start, err)
	}
	return f
}

// repo makes a git repository at dir, as far as this package looks: a
// .git folder with a settings file.
func repo(t *testing.T, dir, config string) string {
	t.Helper()
	write(t, filepath.Join(dir, ".git", "config"), "[core]\n\trepositoryformatversion = 0\n\tbare = false\n"+config)
	write(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	return dir
}

// worktree makes a linked worktree of the repository at main, at dir, the
// way git lays one out: a record in the repository's worktrees folder that
// names the worktree, and a .git file in the worktree that names the
// record.
func worktree(t *testing.T, main, dir, name string) string {
	t.Helper()
	record := filepath.Join(main, ".git", "worktrees", name)
	write(t, filepath.Join(record, "commondir"), "../..\n")
	write(t, filepath.Join(record, "gitdir"), filepath.Join(dir, ".git")+"\n")
	write(t, filepath.Join(record, "HEAD"), "ref: refs/heads/"+name+"\n")
	write(t, filepath.Join(dir, ".git"), "gitdir: "+record+"\n")
	return dir
}

func TestResolvePlainFolder(t *testing.T) {
	tmp := root(t)
	dir := mkdir(t, tmp, "notes", "deep")
	f := resolve(t, dir, filepath.Join(tmp, "home"))
	if f.Start != dir || f.Root != dir || f.Key != dir || f.Repo || f.Worktree || f.SessionOnly || f.GitDir != "" || len(f.Notes) != 0 {
		t.Errorf("a folder outside any repository is its own project and its own key: %+v", f)
	}
}

func TestResolveRepository(t *testing.T) {
	tmp := root(t)
	top := repo(t, filepath.Join(tmp, "proj"), "")
	deep := mkdir(t, top, "internal", "pkg")
	for _, start := range []string{top, deep} {
		f := resolve(t, start, filepath.Join(tmp, "home"))
		if f.Start != start || f.Root != top || f.Key != top || !f.Repo || f.Worktree || f.SessionOnly || f.GitDir != filepath.Join(top, ".git") || f.CommonDir != f.GitDir {
			t.Errorf("started in %s: the project is the repository's top folder: %+v", start, f)
		}
	}
	// A repository's work tree named in its own settings as the folder it
	// is in changes nothing.
	same := repo(t, filepath.Join(tmp, "same"), "\tworktree = "+filepath.Join(tmp, "same")+"\n")
	if f := resolve(t, same, ""); f.Key != same || len(f.Notes) != 0 {
		t.Errorf("core.worktree naming the folder itself: %+v", f)
	}
}

// Worktrees share their repository's trust, but only the ones the
// repository itself lists.
func TestResolveWorktree(t *testing.T) {
	tmp := root(t)
	main := repo(t, filepath.Join(tmp, "main"), "")
	wt := worktree(t, main, filepath.Join(tmp, "feature"), "feature")
	deep := mkdir(t, wt, "sub")
	f := resolve(t, deep, "")
	if f.Root != wt || f.Key != main || !f.Repo || !f.Worktree || f.CommonDir != filepath.Join(main, ".git") || f.GitDir != filepath.Join(main, ".git", "worktrees", "feature") {
		t.Errorf("a worktree the repository lists is keyed on the main checkout: %+v", f)
	}

	// The record may name the worktree by a relative path.
	rel := filepath.Join(tmp, "relative")
	record := filepath.Join(main, ".git", "worktrees", "relative")
	write(t, filepath.Join(record, "commondir"), "../..\n")
	write(t, filepath.Join(record, "gitdir"), "../../../../relative/.git\n")
	write(t, filepath.Join(rel, ".git"), "gitdir: ../main/.git/worktrees/relative\n")
	if f := resolve(t, rel, ""); f.Key != main || !f.Worktree {
		t.Errorf("a worktree recorded by relative paths: %+v", f)
	}
}

// SAFE-8 case 3, and the tricks around it: a folder cannot borrow a
// trusted repository's trust by what it puts in a .git of its own.
func TestResolveRefusesBorrowedTrust(t *testing.T) {
	tmp := root(t)
	main := repo(t, filepath.Join(tmp, "trusted"), "")
	wt := worktree(t, main, filepath.Join(tmp, "trusted-feature"), "feature")
	record := filepath.Join(main, ".git", "worktrees", "feature")

	tricks := map[string]func(evil string){
		// The .git is a link to a real worktree's .git file. A check that
		// follows the link finds the repository listing "it".
		"a .git that is a link to a listed worktree's .git": func(evil string) {
			symlink(t, filepath.Join(wt, ".git"), filepath.Join(evil, ".git"))
		},
		// The .git is a copy of a real worktree's .git file.
		"a .git copied from a listed worktree": func(evil string) {
			write(t, filepath.Join(evil, ".git"), "gitdir: "+record+"\n")
		},
		// The .git names a record the folder made itself, which points at
		// the trusted repository and back at the folder.
		"a record of its own making": func(evil string) {
			fake := filepath.Join(evil, "fake", "worktrees", "x")
			write(t, filepath.Join(fake, "commondir"), filepath.Join(main, ".git")+"\n")
			write(t, filepath.Join(fake, "gitdir"), filepath.Join(evil, ".git")+"\n")
			write(t, filepath.Join(evil, ".git"), "gitdir: "+fake+"\n")
		},
		// The record is reached through a link into the trusted
		// repository, so it resolves to a real record, of another folder.
		"a link into the trusted repository's records": func(evil string) {
			symlink(t, filepath.Join(main, ".git"), filepath.Join(evil, "fake"))
			write(t, filepath.Join(evil, ".git"), "gitdir: "+filepath.Join(evil, "fake", "worktrees", "feature")+"\n")
		},
		// The .git is a link to the trusted repository's .git folder.
		"a .git that is a link to the trusted repository's .git": func(evil string) {
			symlink(t, filepath.Join(main, ".git"), filepath.Join(evil, ".git"))
		},
		// The .git names a git directory that is not there.
		"a .git that names nothing": func(evil string) {
			write(t, filepath.Join(evil, ".git"), "gitdir: "+filepath.Join(tmp, "nowhere")+"\n")
		},
		"a .git that is not a git file": func(evil string) {
			write(t, filepath.Join(evil, ".git"), "hello\n")
		},
	}
	i := 0
	for name, plant := range tricks {
		i++
		evil := mkdir(t, tmp, "evil"+string(rune('0'+i)))
		plant(evil)
		f := resolve(t, evil, "")
		if f.Key != evil || f.Root != evil || f.Worktree {
			t.Errorf("%s: the folder borrowed another's key: %+v", name, f)
		}
		if f.Key == main || f.Key == wt {
			t.Errorf("%s: keyed on the trusted repository", name)
		}
		if len(f.Notes) == 0 {
			t.Errorf("%s: why the shortcut was refused is said: %+v", name, f)
		}
	}
	// The real worktree still shares.
	if f := resolve(t, wt, ""); f.Key != main {
		t.Errorf("the listed worktree: %+v", f)
	}
}

// SAFE-8 case 1: a downloaded repository's own git settings place its work
// tree elsewhere. That must not move the idea of where the project is: the
// folder is the one gnosh was started in, and it is keyed on itself.
func TestResolveGitSettingsCannotMoveTheProject(t *testing.T) {
	tmp := root(t)
	trusted := repo(t, filepath.Join(tmp, "trusted"), "")
	for name, config := range map[string]string{
		"a work tree somewhere else":    "\tworktree = " + trusted + "\n",
		"a relative work tree":          "\tworktree = ../../trusted\n",
		"a work tree that is not there": "\tworktree = /nowhere/at/all\n",
		"settings that pull in a file":  "[include]\n\tpath = ../evil.gitconfig\n",
		"settings that cannot be read":  "[core\n\tworktree = x\n",
		"a conditional include":         "[includeIf \"gitdir:/\"]\n\tpath = x\n",
		"the setting in upper case":     "[CORE]\n\tWorkTree = " + trusted + "\n",
	} {
		evil := repo(t, filepath.Join(tmp, strings.ReplaceAll(name, " ", "-")), config)
		start := mkdir(t, evil, "sub")
		f := resolve(t, start, "")
		if f.Root != start || f.Key != start || f.Key == trusted {
			t.Errorf("%s: the folder must be the one gnosh was started in: %+v", name, f)
		}
		if len(f.Notes) != 1 || !strings.Contains(f.Notes[0], "work tree") {
			t.Errorf("%s: why is said: %q", name, f.Notes)
		}
	}
	// The same setting in the per-worktree settings file.
	evil := repo(t, filepath.Join(tmp, "per-worktree"), "")
	write(t, filepath.Join(evil, ".git", "config.worktree"), "[core]\n\tworktree = "+trusted+"\n")
	if f := resolve(t, evil, ""); len(f.Notes) != 1 || f.Key != evil {
		t.Errorf("config.worktree: %+v", f)
	}
}

// A repository inside another (a submodule) is a repository of its own.
func TestResolveSubmodule(t *testing.T) {
	tmp := root(t)
	super := repo(t, filepath.Join(tmp, "super"), "")
	sub := filepath.Join(super, "vendor", "lib")
	write(t, filepath.Join(super, ".git", "modules", "lib", "config"), "[core]\n\tworktree = ../../../vendor/lib\n")
	write(t, filepath.Join(sub, ".git"), "gitdir: ../../.git/modules/lib\n")
	f := resolve(t, mkdir(t, sub, "src"), "")
	if f.Root != sub || f.Key != sub || !f.Repo || f.Worktree || len(f.Notes) != 0 {
		t.Errorf("a submodule is keyed on itself: %+v", f)
	}
	if f.GitDir != filepath.Join(super, ".git", "modules", "lib") {
		t.Errorf("its git directory is the one git would read: %q", f.GitDir)
	}
}

// The home directory, and anything that holds it, is trusted for one
// session at a time.
func TestResolveHome(t *testing.T) {
	tmp := root(t)
	home := mkdir(t, tmp, "users", "dev")
	if f := resolve(t, home, home); !f.SessionOnly {
		t.Errorf("the home directory: %+v", f)
	}
	if f := resolve(t, filepath.Dir(home), home); !f.SessionOnly {
		t.Errorf("a folder that holds the home directory: %+v", f)
	}
	if f := resolve(t, mkdir(t, home, "src", "proj"), home); f.SessionOnly {
		t.Errorf("a folder inside the home directory is an ordinary folder: %+v", f)
	}
	// A home directory kept in git (dotfiles) is not the project of every
	// folder under it.
	repo(t, home, "")
	notes := mkdir(t, home, "notes")
	if f := resolve(t, notes, home); f.Root != notes || f.Key != notes || f.Repo || f.SessionOnly || len(f.Notes) != 1 {
		t.Errorf("a folder under a home directory that is a repository: %+v", f)
	}
	if f := resolve(t, home, home); !f.SessionOnly || f.Key != home {
		t.Errorf("the home directory itself, as a repository: %+v", f)
	}
	// A home directory reached through a link is still the home directory.
	symlink(t, home, filepath.Join(tmp, "homelink"))
	if f := resolve(t, home, filepath.Join(tmp, "homelink")); !f.SessionOnly {
		t.Errorf("home given through a link: %+v", f)
	}
}

func TestResolveLinksAndMistakes(t *testing.T) {
	tmp := root(t)
	real := repo(t, filepath.Join(tmp, "real"), "")
	symlink(t, real, filepath.Join(tmp, "link"))
	if f := resolve(t, filepath.Join(tmp, "link"), ""); f.Start != real || f.Key != real {
		t.Errorf("a folder reached through a link is the folder it leads to: %+v", f)
	}
	write(t, filepath.Join(tmp, "file"), "x")
	for name, start := range map[string]string{"a relative path": "proj", "a folder that is not there": filepath.Join(tmp, "nope"), "a file": filepath.Join(tmp, "file")} {
		if _, err := Resolve(start, ""); err == nil {
			t.Errorf("%s: Resolve should say it cannot", name)
		}
	}
}

func TestParseGitConfig(t *testing.T) {
	text := string(rune(0xFEFF)) + "# a comment\n; another\n[core]\n\tbare = false\n\tfsmonitor = \"./watch.sh\" # why\n\tignorecase\n" +
		"[filter \"lfs\"]\n\tsmudge = git-lfs smudge -- %f\n\tclean = a \\\nb\n" +
		"[remote \"origin\"] url = https://example.com/x.git\n[alias]\n\tst = status\n\tboom = \"!sh -c 'x'\"\n[diff.old]\n\ttextconv = cat\n"
	settings, ok := parseGitConfig(text)
	if !ok {
		t.Fatalf("a git config file of ordinary shape must be read")
	}
	var got []string
	for _, s := range settings {
		got = append(got, s.name()+"="+s.value)
	}
	want := []string{
		"core.bare=false", "core.fsmonitor=./watch.sh", "core.ignorecase=true",
		"filter.lfs.smudge=git-lfs smudge -- %f", "filter.lfs.clean=a b",
		"remote.origin.url=https://example.com/x.git", "alias.st=status", "alias.boom=!sh -c 'x'", "diff.old.textconv=cat",
	}
	if !slices.Equal(got, want) {
		t.Errorf("settings:\n got %q\nwant %q", got, want)
	}
	for _, bad := range []string{"[core\n", "key = value\n", "[core]\n= x\n", "[filter \"x]\n", "[]\na = b\n"} {
		if _, ok := parseGitConfig(bad); ok {
			t.Errorf("%q should not be read as a settings file", bad)
		}
	}
}

// A subsection may be written with a tab before it: git takes any space
// between the section's name and the quoted subsection, so a section
// header holding a tab still names filter.lfs, and git runs what
// filter.lfs.clean says.
func TestParseGitConfigTabbedSubsection(t *testing.T) {
	t.Parallel()
	text := "[filter\t\"lfs\"]\n\tclean = ./evil.sh\n[includeIf\t\"gitdir:/\"]\n\tpath = ../evil.gitconfig\n"
	settings, ok := parseGitConfig(text)
	if !ok {
		t.Fatalf("settings whose subsections are written with a tab must be read")
	}
	var got []string
	for _, s := range settings {
		got = append(got, s.name()+"="+s.value)
	}
	want := []string{"filter.lfs.clean=./evil.sh", "includeif.gitdir:/.path=../evil.gitconfig"}
	if !slices.Equal(got, want) {
		t.Errorf("settings:\n got %q\nwant %q", got, want)
	}
	for _, s := range settings {
		if !runsProgram(s) {
			t.Errorf("%s names a program git would run", s.name())
		}
	}
	dir := repo(t, filepath.Join(root(t), "tabbed"), "[filter\t\"lfs\"]\n\tclean = ./evil.sh\n")
	if list := GitPrograms(resolve(t, dir, "")); len(list) != 1 || !strings.Contains(list[0].Text, "git filter.lfs.clean = ") {
		t.Errorf("the trust question must name what a tabbed subsection holds: %+v", list)
	}
}

// What a repository's own git settings name that git would run: the trust
// question lists it, and it is found without running git.
func TestGitPrograms(t *testing.T) {
	tmp := root(t)
	if got := GitPrograms(resolve(t, mkdir(t, tmp, "plain"), "")); got != nil {
		t.Errorf("outside a repository: %+v", got)
	}
	clean := repo(t, filepath.Join(tmp, "clean"), "\tfsmonitor = true\n[user]\n\tname = Dev\n[remote \"origin\"]\n\turl = https://example.com/x.git\n[alias]\n\tst = status\n[pager]\n\tlog = false\n")
	write(t, filepath.Join(clean, ".git", "hooks", "post-index-change.sample"), "#!/bin/sh\n")
	write(t, filepath.Join(clean, ".git", "hooks", "pre-commit"), "#!/bin/sh\n")
	if got := GitPrograms(resolve(t, clean, "")); len(got) != 0 {
		t.Errorf("a repository whose settings name no program run on a look: %+v", got)
	}

	settings := map[string]string{
		"core.fsmonitor":            "[core]\n\tfsmonitor = ./evil.sh\n",
		"core.sshcommand":           "[core]\n\tsshCommand = ./evil.sh\n",
		"core.hookspath":            "[core]\n\thooksPath = ./hooks\n",
		"core.pager":                "[core]\n\tpager = ./evil.sh\n",
		"core.gitproxy":             "[core]\n\tgitProxy = ./evil.sh\n",
		"filter.x.smudge":           "[filter \"x\"]\n\tsmudge = ./evil.sh\n",
		"filter.x.process":          "[filter \"x\"]\n\tprocess = ./evil.sh\n",
		"diff.x.textconv":           "[diff \"x\"]\n\ttextconv = ./evil.sh\n",
		"diff.external":             "[diff]\n\texternal = ./evil.sh\n",
		"merge.x.driver":            "[merge \"x\"]\n\tdriver = ./evil.sh\n",
		"gpg.program":               "[gpg]\n\tprogram = ./evil.sh\n",
		"gpg.ssh.program":           "[gpg \"ssh\"]\n\tprogram = ./evil.sh\n",
		"credential.helper":         "[credential]\n\thelper = ./evil.sh\n",
		"remote.origin.uploadpack":  "[remote \"origin\"]\n\tuploadpack = ./evil.sh\n",
		"remote.origin.receivepack": "[remote \"origin\"]\n\treceivepack = ./evil.sh\n",
		"pager.status":              "[pager]\n\tstatus = ./evil.sh\n",
		"alias.st":                  "[alias]\n\tst = !./evil.sh\n",
		"include.path":              "[include]\n\tpath = ../evil.gitconfig\n",
	}
	i := 0
	for name, config := range settings {
		i++
		dir := repo(t, filepath.Join(tmp, "r"+string(rune('a'+i))), config)
		got := GitPrograms(resolve(t, dir, ""))
		if len(got) != 1 || got[0].Kind != KindGitProgram || !strings.Contains(got[0].Text, "git "+name+" = ") || got[0].File != filepath.Join(dir, ".git", "config") || got[0].Line == 0 {
			t.Errorf("%s: %+v", name, got)
		}
	}

	// The hook that fires when the index is written, which a status does.
	hooked := repo(t, filepath.Join(tmp, "hooked"), "")
	write(t, filepath.Join(hooked, ".git", "hooks", "post-index-change"), "#!/bin/sh\n./evil.sh\n")
	if got := GitPrograms(resolve(t, hooked, "")); len(got) != 1 || !strings.Contains(got[0].Text, "git hook post-index-change") {
		t.Errorf("a hook that runs on a look: %+v", got)
	}
	// Settings that cannot be understood are taken to name programs.
	broken := repo(t, filepath.Join(tmp, "broken"), "[filter\n")
	if got := GitPrograms(resolve(t, broken, "")); len(got) != 1 || !strings.Contains(got[0].Text, "could not be understood") {
		t.Errorf("settings that cannot be understood: %+v", got)
	}
	// A worktree is looked at through its repository's settings.
	main := repo(t, filepath.Join(tmp, "main"), "[core]\n\tfsmonitor = ./evil.sh\n")
	wt := worktree(t, main, filepath.Join(tmp, "wt"), "wt")
	if got := GitPrograms(resolve(t, wt, "")); len(got) != 1 || got[0].File != filepath.Join(main, ".git", "config") {
		t.Errorf("a worktree's repository settings: %+v", got)
	}
}

// FuzzGitConfig: the settings reader never panics.
func FuzzGitConfig(f *testing.F) {
	for _, seed := range []string{"", "[core]\n\tworktree = x\n", "[a \"b\\\"c\"]\nk = \"v\" ; c\n", "[x]\nk = a \\\n b\n", "[", "]", "= =\n", "[a.b]\nk\n", string(rune(0xFEFF)) + "[c]\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		settings, ok := parseGitConfig(text)
		for _, s := range settings {
			if !ok || s.section == "" || s.key == "" || s.line < 1 {
				t.Fatalf("a setting without a section, a key or a line: %+v (ok %v) for %q", s, ok, text)
			}
			_ = runsProgram(s)
		}
	})
}
