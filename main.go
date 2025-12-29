package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func main() {
	cmd := newRootCmd()
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var (
		addr     string
		count    int
		timeout  time.Duration
		interval time.Duration
		password string
		source   string
		sourceIP string
	)

	cmd := &cobra.Command{
		Use:   "redisping [host:port]",
		Short: "Measure Redis PING/PONG RTT",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := strings.TrimSpace(addr)
			if target == "" && len(args) > 0 {
				target = strings.TrimSpace(args[0])
			}
			if target == "" {
				return fmt.Errorf("target required; set --addr or provide host:port")
			}
			if count <= 0 {
				return fmt.Errorf("--count must be > 0")
			}
			return runPing(target, count, timeout, interval, password, source, sourceIP)
		},
	}

	cmd.Flags().StringVarP(&addr, "addr", "a", "", "target host:port (positional also accepted)")
	cmd.Flags().IntVar(&count, "count", 20, "number of ping attempts")
	cmd.Flags().DurationVar(&timeout, "timeout", time.Second, "per-request timeout")
	cmd.Flags().DurationVar(&interval, "interval", 100*time.Millisecond, "delay between requests")
	cmd.Flags().StringVar(&password, "password", "", "Redis AUTH password (optional)")
	cmd.Flags().StringVar(&source, "source", "", "identifier of source pod/node (display only)")
	cmd.Flags().StringVar(&sourceIP, "source-ip", "", "source IP to display in output")

	cmd.AddCommand(newAttachCmd())
	cmd.AddCommand(newCompletionCmd(cmd))

	return cmd
}

func runPing(target string, count int, timeout, interval time.Duration, password, source, sourceIP string) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.Dial("tcp", target)
	if err != nil {
		return fmt.Errorf("dial error: %w", err)
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)

	if password != "" {
		if err := doAuth(conn, reader, timeout, password); err != nil {
			return fmt.Errorf("auth error: %w", err)
		}
	}

	fmt.Printf("redisping -> %s (count=%d timeout=%s interval=%s auth=%t", target, count, timeout.String(), interval.String(), password != "")
	if source != "" {
		fmt.Printf(" source=%s", source)
	}
	if sourceIP != "" {
		fmt.Printf(" source-ip=%s", sourceIP)
	}
	fmt.Println(")")

	var samples []time.Duration
	var failures int
	last := ""

	interrupted := false
	for i := 0; i < count; i++ {
		select {
		case <-sig:
			interrupted = true
			last = "interrupted"
			renderProgress(i, count, len(samples), failures, last, source, sourceIP, target)
			break
		default:
		}

		if i > 0 && interval > 0 {
			time.Sleep(interval)
		}

		start := time.Now()
		if err := conn.SetDeadline(start.Add(timeout)); err != nil {
			return fmt.Errorf("set deadline: %w", err)
		}
		if err := sendPing(conn); err != nil {
			failures++
			last = fmt.Sprintf("fail: %v", err)
			renderProgress(i+1, count, len(samples), failures, last, source, sourceIP, target)
			continue
		}

		pong, err := readSimpleReply(reader)
		if err != nil {
			failures++
			last = fmt.Sprintf("fail: %v", err)
			renderProgress(i+1, count, len(samples), failures, last, source, sourceIP, target)
			continue
		}
		if pong != "PONG" {
			failures++
			last = fmt.Sprintf("fail: unexpected reply: %q", pong)
			renderProgress(i+1, count, len(samples), failures, last, source, sourceIP, target)
			continue
		}

		dur := time.Since(start)
		samples = append(samples, dur)
		last = fmt.Sprintf("%8.3f ms", float64(dur.Microseconds())/1000)
		renderProgress(i+1, count, len(samples), failures, last, source, sourceIP, target)
	}

	fmt.Println()
	if interrupted {
		fmt.Println("interrupted; printing partial summary")
	}

	summarize(samples, failures)

	if len(samples) == 0 {
		return fmt.Errorf("no successful replies")
	}
	return nil
}

func doAuth(conn net.Conn, reader *bufio.Reader, timeout time.Duration, password string) error {
	conn.SetDeadline(time.Now().Add(timeout))
	if err := sendCommand(conn, "AUTH", password); err != nil {
		return err
	}
	resp, err := readSimpleReply(reader)
	if err != nil {
		return err
	}
	if resp != "OK" {
		return fmt.Errorf("unexpected auth reply: %s", resp)
	}
	return nil
}

func sendPing(conn net.Conn) error {
	return sendCommand(conn, "PING")
}

func sendCommand(conn net.Conn, parts ...string) error {
	if len(parts) == 0 {
		return errors.New("empty command")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(parts))
	for _, p := range parts {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(p), p)
	}
	_, err := conn.Write([]byte(b.String()))
	return err
}

func readSimpleReply(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) < 3 { // at least +X\r\n or -\r\n
		return "", fmt.Errorf("short reply: %q", line)
	}
	prefix := line[0]
	payload := strings.TrimSuffix(strings.TrimSuffix(line[1:], "\n"), "\r")
	switch prefix {
	case '+':
		return payload, nil
	case '-':
		return "", fmt.Errorf("error reply: %s", payload)
	default:
		return "", fmt.Errorf("unexpected reply type: %q", prefix)
	}
}

func summarize(samples []time.Duration, failures int) {
	total := len(samples) + failures
	fmt.Println("--- summary ---")
	fmt.Printf("sent=%d recv=%d loss=%.2f%%\n", total, len(samples), lossPercent(total, failures))

	if len(samples) == 0 {
		fmt.Println("no successful replies")
		return
	}

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	min := samples[0]
	max := samples[len(samples)-1]
	avg := average(samples)
	p50 := percentile(samples, 50)
	p90 := percentile(samples, 90)
	p99 := percentile(samples, 99)

	fmt.Printf("min/avg/max = %.3f/%.3f/%.3f ms\n", ms(min), ms(avg), ms(max))
	fmt.Printf("p50/p90/p99 = %.3f/%.3f/%.3f ms\n", ms(p50), ms(p90), ms(p99))
}

func lossPercent(total, failures int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(failures) / float64(total)
}

func average(samples []time.Duration) time.Duration {
	var sum time.Duration
	for _, s := range samples {
		sum += s
	}
	return time.Duration(int64(sum) / int64(len(samples)))
}

func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	if p <= 0 {
		return samples[0]
	}
	if p >= 100 {
		return samples[len(samples)-1]
	}
	pos := (p / 100) * float64(len(samples)-1)
	lower := int(math.Floor(pos))
	upper := int(math.Ceil(pos))
	if upper >= len(samples) {
		upper = len(samples) - 1
	}
	if lower == upper {
		return samples[lower]
	}
	ratio := pos - float64(lower)
	interpolated := float64(samples[lower]) + (float64(samples[upper]-samples[lower]) * ratio)
	return time.Duration(interpolated)
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func renderProgress(done, total, ok, fail int, last, source, sourceIP, target string) {
	const maxLen = 32
	if len(last) > maxLen {
		last = last[:maxLen-3] + "..."
	}
	src := source
	if src == "" {
		src = "-"
	}
	if sourceIP != "" {
		src = fmt.Sprintf("%s/%s", src, sourceIP)
	}
	fmt.Printf("\r[%3d/%d] src=%-24s dst=%-21s last=%-35s ok=%d fail=%d", done, total, src, target, last, ok, fail)
}

func newAttachCmd() *cobra.Command {
	var (
		podFlag       string
		namespace     string
		addr          string
		count         int
		timeout       time.Duration
		interval      time.Duration
		password      string
		kubectlBinary string
		container     string
	)

	cmd := &cobra.Command{
		Use:   "attach --pod <pod> [host:port]",
		Short: "Copy redisping into a pod and run it via kubectl exec",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := strings.TrimSpace(addr)
			if target == "" && len(args) > 0 {
				target = strings.TrimSpace(args[0])
			}
			if target == "" {
				return fmt.Errorf("target required; set --addr or provide host:port")
			}
			if podFlag == "" {
				return fmt.Errorf("--pod is required (format: [ns/]pod)")
			}
			podNS, podName := splitPodRef(namespace, podFlag)
			if count <= 0 {
				return fmt.Errorf("--count must be > 0")
			}
			return runAttach(podNS, podName, container, target, count, timeout, interval, password, kubectlBinary)
		},
	}

	cmd.Flags().StringVar(&podFlag, "pod", "", "target pod (format: [namespace/]name)")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "pod namespace (ignored if pod includes namespace)")
	cmd.Flags().StringVarP(&container, "container", "c", "redis", "container name")
	cmd.Flags().StringVarP(&addr, "addr", "a", "", "target host:port inside cluster (positional also accepted)")
	cmd.Flags().IntVar(&count, "count", 20, "number of ping attempts")
	cmd.Flags().DurationVar(&timeout, "timeout", time.Second, "per-request timeout")
	cmd.Flags().DurationVar(&interval, "interval", 100*time.Millisecond, "delay between requests")
	cmd.Flags().StringVar(&password, "password", "", "Redis AUTH password (optional)")
	cmd.Flags().StringVar(&kubectlBinary, "kubectl", "kubectl", "kubectl binary path")

	return cmd
}

func runAttach(namespace, pod, container, target string, count int, timeout, interval time.Duration, password, kubectlPath string) error {
	if _, err := exec.LookPath(kubectlPath); err != nil {
		return fmt.Errorf("kubectl not found: %w", err)
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find self executable: %w", err)
	}
	bin, err := os.ReadFile(self)
	if err != nil {
		return fmt.Errorf("cannot read self executable: %w", err)
	}

	remoteArgs := []string{"/tmp/redisping", "--addr", target, "--count", strconv.Itoa(count), "--timeout", timeout.String(), "--interval", interval.String()}
	if password != "" {
		remoteArgs = append(remoteArgs, "--password", password)
	}

	source := fmt.Sprintf("%s/%s", namespace, pod)
	base := joinShell(remoteArgs)
	remoteCmd := fmt.Sprintf("cat >/tmp/redisping && chmod +x /tmp/redisping && SRC_IP=$(hostname -i | awk '{print $1}') && %s --source %s --source-ip \"$SRC_IP\"; rc=$?; rm -f /tmp/redisping; exit $rc", base, shellQuote(source))

	kargs := []string{"exec", "-i", "-n", namespace, pod}
	if container != "" {
		kargs = append(kargs, "-c", container)
	}
	kargs = append(kargs, "--", "/bin/sh", "-c", remoteCmd)

	kcmd := exec.Command(kubectlPath, kargs...)
	kcmd.Stdin = bytes.NewReader(bin)
	kcmd.Stdout = os.Stdout
	kcmd.Stderr = os.Stderr
	return kcmd.Run()
}

func splitPodRef(defaultNS, ref string) (string, string) {
	if strings.Contains(ref, "/") {
		parts := strings.SplitN(ref, "/", 2)
		return parts[0], parts[1]
	}
	return defaultNS, ref
}

func joinShell(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, shellQuote(a))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	// simple POSIX-style single-quote escaping
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func newCompletionCmd(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:       "completion [bash|zsh|fish|powershell]",
		Short:     "Generate shell completion scripts",
		Args:      cobra.ExactValidArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return root.GenBashCompletion(os.Stdout)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(os.Stdout)
			default:
				return fmt.Errorf("unsupported shell: %s", args[0])
			}
		},
	}
}
