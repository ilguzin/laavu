package install

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ilguzin/laavu/internal/release"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// Установка с ноутбука: laavu install --ssh пользователь@IP. Встроенный SSH-клиент,
// отпечаток хоста при первом подключении, пароль только в памяти процесса.

// RemoteOptions — то, что задаётся только для установки по SSH.
type RemoteOptions struct {
	Target   string // пользователь@хост[:порт]
	Identity string // закрытый ключ для входа; "none" — только пароль
	Yes      bool   // принять отпечаток хоста без вопроса (тесты)
	// Release — каталог или https-адрес подписанного релиза. Пусто — бинарники
	// рядом с запущенным laavu (сборка разработчика, без подписи).
	Release string
	// OwnerPub — открытый ключ владельца; пусто — ~/.ssh/id_ed25519.pub или новый ключ.
	OwnerPub string
	Stdin    io.Reader
	Out      io.Writer
}

func knownHostsPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "laavu", "known_hosts")
}

func splitTarget(t string) (user, addr string, err error) {
	user, host, ok := strings.Cut(t, "@")
	if !ok || user == "" || host == "" {
		return "", "", errors.New("адрес вида пользователь@IP[:порт]")
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "22")
	}
	return user, host, nil
}

// hostKeyCallback показывает отпечаток при первом подключении и запоминает его.
// Если ключ хоста потом сменился — отказ: это либо переустановка, либо атака.
func hostKeyCallback(ro *RemoteOptions, in *bufio.Reader) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		line := hostname + " " + fp
		path := knownHostsPath()
		if b, err := os.ReadFile(path); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				h, f, ok := strings.Cut(l, " ")
				if !ok || h != hostname {
					continue
				}
				if f == fp {
					return nil
				}
				return fmt.Errorf("ключ хоста %s изменился (был %s, стал %s). Если сервер переустановлен, удалите строку из %s", hostname, f, fp, path)
			}
		}
		fmt.Fprintf(ro.Out, "Первое подключение к %s.\nОтпечаток ключа сервера: %s\nСверьте его с панелью хостера.\n", hostname, fp)
		if !ro.Yes {
			fmt.Fprint(ro.Out, "Продолжить? [y/N] ")
			ans, _ := in.ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" && a != "д" && a != "да" {
				return errors.New("подключение отменено")
			}
		}
		os.MkdirAll(filepath.Dir(path), 0o700)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintln(f, line)
		return err
	}
}

func readPassword(ro *RemoteOptions, in *bufio.Reader, prompt string) (string, error) {
	fmt.Fprint(ro.Out, prompt)
	if f, ok := ro.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(ro.Out)
		return string(b), err
	}
	// Не терминал: пароль одной строкой со stdin (для автотестов).
	s, err := in.ReadString('\n')
	fmt.Fprintln(ro.Out)
	return strings.TrimRight(s, "\r\n"), err
}

func authMethods(ro *RemoteOptions, in *bufio.Reader, user, addr string) []ssh.AuthMethod {
	var methods []ssh.AuthMethod
	keyPath := ro.Identity
	if keyPath == "" {
		home, _ := os.UserHomeDir()
		keyPath = filepath.Join(home, ".ssh", "id_ed25519")
	}
	if keyPath != "none" {
		if b, err := os.ReadFile(keyPath); err == nil {
			if signer, err := ssh.ParsePrivateKey(b); err == nil {
				methods = append(methods, ssh.PublicKeys(signer))
			}
		}
	}
	var pw string
	asked := false
	ask := func() (string, error) {
		if !asked {
			asked = true
			var err error
			pw, err = readPassword(ro, in, fmt.Sprintf("Пароль %s@%s: ", user, addr))
			if err != nil {
				return "", err
			}
		}
		return pw, nil
	}
	methods = append(methods, ssh.PasswordCallback(ask))
	methods = append(methods, ssh.KeyboardInteractive(func(name, instr string, qs []string, echos []bool) ([]string, error) {
		ans := make([]string, len(qs))
		for i := range qs {
			p, err := ask()
			if err != nil {
				return nil, err
			}
			ans[i] = p
		}
		return ans, nil
	}))
	return methods
}

// Dial подключается к серверу.
func Dial(ro *RemoteOptions, in *bufio.Reader) (*ssh.Client, string, error) {
	user, addr, err := splitTarget(ro.Target)
	if err != nil {
		return nil, "", err
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods(ro, in, user, addr),
		HostKeyCallback: hostKeyCallback(ro, in),
		Timeout:         15 * time.Second,
	}
	c, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, user, fmt.Errorf("ssh %s: %w", addr, err)
	}
	return c, user, nil
}

// lockedWriter — общий писатель для stdout и stderr сессии. Без него два
// копировщика писали бы в один bytes.Buffer одновременно (через его ReadFrom)
// и теряли вывод.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// remoteRun выполняет команду и копирует её вывод в out.
func remoteRun(c *ssh.Client, cmd string, stdin io.Reader, out io.Writer) error {
	s, err := c.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	s.Stdin = stdin
	lw := &lockedWriter{w: out}
	s.Stdout = lw
	s.Stderr = lw
	return s.Run(cmd)
}

func remoteOutput(c *ssh.Client, cmd string) (string, error) {
	var b bytes.Buffer
	err := remoteRun(c, cmd, nil, &b)
	return strings.TrimSpace(b.String()), err
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ownerPublicKey — открытый ключ владельца: заданный, ~/.ssh/id_ed25519.pub или новый.
func ownerPublicKey(ro *RemoteOptions, host string) (string, string, error) {
	if ro.OwnerPub != "" {
		b, err := os.ReadFile(ro.OwnerPub)
		if err != nil {
			return "", "", err
		}
		return strings.TrimSpace(string(b)), strings.TrimSuffix(ro.OwnerPub, ".pub"), nil
	}
	home, _ := os.UserHomeDir()
	def := filepath.Join(home, ".ssh", "id_ed25519")
	if b, err := os.ReadFile(def + ".pub"); err == nil {
		return strings.TrimSpace(string(b)), def, nil
	}
	// Ключа нет — создаём отдельный для этого сервера.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	blk, err := ssh.MarshalPrivateKey(priv, "laavu "+host)
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(home, ".ssh", "laavu_"+strings.NewReplacer(":", "_", ".", "_").Replace(host))
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		return "", "", err
	}
	sp, _ := ssh.NewPublicKey(pub)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " laavu"
	os.WriteFile(path+".pub", []byte(line+"\n"), 0o644)
	return line, path, nil
}

var archMap = map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}

// RemoteInstall — laavu install --ssh.
func RemoteInstall(ro RemoteOptions, o Options, passArgs []string) error {
	if ro.Out == nil {
		ro.Out = os.Stdout
	}
	if ro.Stdin == nil {
		ro.Stdin = os.Stdin
	}
	in := bufio.NewReader(ro.Stdin)
	out := ro.Out

	// 1. Подпись релиза проверяется на ноутбуке до копирования.
	var ver *release.Verified
	if ro.Release != "" {
		pub, err := release.PinnedKey()
		if err != nil {
			return err
		}
		if ver, err = release.Verify(release.Source(ro.Release), pub); err != nil {
			return err
		}
		fmt.Fprintf(out, "Релиз %s: подпись проверена.\n", ver.Manifest.Version)
	}

	c, user, err := Dial(&ro, in)
	if err != nil {
		return err
	}
	defer c.Close()
	_, addr, _ := splitTarget(ro.Target)
	host, _, _ := net.SplitHostPort(addr)
	fmt.Fprintf(out, "Подключился к %s.\n", addr)

	sudo := ""
	if user != "root" {
		if _, err := remoteOutput(c, "sudo -n true"); err != nil {
			return errors.New("нужен root или пользователь с sudo без пароля")
		}
		sudo = "sudo -n "
	}
	arch, err := remoteOutput(c, "uname -m")
	if err != nil {
		return err
	}
	goarch := archMap[arch]
	if goarch == "" {
		return fmt.Errorf("архитектура сервера %s не поддерживается", arch)
	}

	// 2. Бинарники: из подписанного релиза или рядом с запущенным laavu.
	local := map[string]string{}
	tmpDir, err := os.MkdirTemp("", "laavu-install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	for _, name := range []string{"laavu", "laavud"} {
		if ver != nil {
			p := filepath.Join(tmpDir, name)
			if err := ver.Fetch(name+"-linux-"+goarch, p); err != nil {
				return err
			}
			local[name] = p
			continue
		}
		if runtime.GOOS != "linux" || runtime.GOARCH != goarch {
			return fmt.Errorf("эта сборка laavu под %s/%s, а сервер linux/%s: укажите --release", runtime.GOOS, runtime.GOARCH, goarch)
		}
		self, _ := os.Executable()
		local[name] = filepath.Join(filepath.Dir(self), name)
	}
	if ver == nil {
		fmt.Fprintln(out, "Внимание: ставлю локальную сборку без подписи релиза.")
	}
	remoteDir, err := remoteOutput(c, "mktemp -d /tmp/laavu-install.XXXXXX")
	if err != nil {
		return err
	}
	defer remoteRun(c, sudo+"rm -rf "+shellQuote(remoteDir), nil, io.Discard)
	for name, p := range local {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		err = remoteRun(c, "cat > "+shellQuote(remoteDir+"/"+name)+" && chmod 755 "+shellQuote(remoteDir+"/"+name), f, out)
		f.Close()
		if err != nil {
			return fmt.Errorf("копирование %s: %w", name, err)
		}
	}
	// 3. Вторая проверка подписи — уже на сервере, его собственным laavu.
	if ver != nil {
		for _, f := range []string{release.ManifestName, release.SignatureName} {
			b, err := release.Source(ro.Release).ReadFile(f)
			if err != nil {
				return err
			}
			if err := remoteRun(c, "cat > "+shellQuote(remoteDir+"/"+f), bytes.NewReader(b), out); err != nil {
				return err
			}
		}
		key, _ := release.PinnedKeyString()
		cmd := fmt.Sprintf("%s/laavu release verify --dir %s --key %s --file laavud-linux-%s=%s/laavud --file laavu-linux-%s=%s/laavu",
			remoteDir, remoteDir, shellQuote(key), goarch, remoteDir, goarch, remoteDir)
		if err := remoteRun(c, cmd, nil, out); err != nil {
			return fmt.Errorf("проверка подписи на сервере: %w", err)
		}
	}

	// 4. Установка; ключ владельца — до запрета пароля.
	ownerPub, ownerKeyPath, err := ownerPublicKey(&ro, host)
	if err != nil {
		return err
	}
	args := append([]string{}, passArgs...)
	args = append(args, "--owner-key", ownerPub)
	if user != "root" {
		args = append(args, "--owner-user", user)
	}
	if k, err := release.PinnedKeyString(); err == nil {
		args = append(args, "--release-key", k)
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	fmt.Fprintln(out, "Ставлю Laavu на сервер…")
	var logBuf bytes.Buffer
	if err := remoteRun(c, sudo+remoteDir+"/laavu install "+strings.Join(quoted, " "), nil, io.MultiWriter(out, &logBuf)); err != nil {
		return fmt.Errorf("установка на сервере: %w", err)
	}

	// 5. Вход по ключу проверяется новым соединением; только потом пароль запрещается.
	signerB, err := os.ReadFile(ownerKeyPath)
	if err != nil {
		return fmt.Errorf("закрытый ключ владельца %s: %w", ownerKeyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(signerB)
	if err != nil {
		fmt.Fprintf(out, "Ключ %s защищён паролем — вход по ключу не проверить, вход по паролю оставлен.\n", ownerKeyPath)
		return nil
	}
	kc, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback(&RemoteOptions{Yes: true, Out: io.Discard}, in), Timeout: 15 * time.Second})
	if err != nil {
		fmt.Fprintf(out, "Вход по ключу не прошёл (%v) — вход по паролю оставлен.\n", err)
		return nil
	}
	hargs := []string{"--owner-key", shellQuote(ownerPub)}
	if user != "root" {
		hargs = append(hargs, "--owner-user", shellQuote(user))
	}
	err = remoteRun(kc, sudo+"/usr/local/bin/laavu harden-ssh "+strings.Join(hargs, " "), nil, io.Discard)
	kc.Close()
	if err != nil {
		return fmt.Errorf("запрет входа по паролю: %w", err)
	}
	fmt.Fprintf(out, "\nВход на сервер теперь только по ключу: ssh -i %s %s@%s\n", ownerKeyPath, user, host)
	return nil
}
