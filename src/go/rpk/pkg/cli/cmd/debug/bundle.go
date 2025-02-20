// Copyright 2021 Vectorized, Inc.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.md
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0

//go:build linux
// +build linux

package debug

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/beevik/ntp"
	"github.com/docker/go-units"
	"github.com/hashicorp/go-multierror"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/vectorizedio/redpanda/src/go/rpk/pkg/api/admin"
	"github.com/vectorizedio/redpanda/src/go/rpk/pkg/cli/cmd/common"
	"github.com/vectorizedio/redpanda/src/go/rpk/pkg/config"
	"github.com/vectorizedio/redpanda/src/go/rpk/pkg/kafka"
	"github.com/vectorizedio/redpanda/src/go/rpk/pkg/out"
	"github.com/vectorizedio/redpanda/src/go/rpk/pkg/system"
	"github.com/vectorizedio/redpanda/src/go/rpk/pkg/system/syslog"
	"gopkg.in/yaml.v2"
)

// Use the same date specs as journalctl (see `man journalctl`).
const timeHelpText = `(journalctl date format, e.g. YYYY-MM-DD)`

type step func() error

type stepParams struct {
	fs      afero.Fs
	m       sync.Mutex
	w       *zip.Writer
	timeout time.Duration
}

type fileInfo struct {
	Size     string `json:"size"`
	Mode     string `json:"mode"`
	Symlink  string `json:"symlink,omitempty"`
	Error    string `json:"error,omitempty"`
	Modified string `json:"modified"`
	User     string `json:"user"`
	Group    string `json:"group"`
}

type limitedWriter struct {
	w          io.Writer
	limitBytes int
	accumBytes int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	limitReached := false
	if l.accumBytes+len(p) > l.limitBytes {
		p = p[:l.limitBytes-l.accumBytes]
		limitReached = true
	}

	n, err := l.w.Write(p)
	if err != nil {
		return n, err
	}

	l.accumBytes += n

	if limitReached {
		return n, errors.New("output size limit reached")
	}
	return n, nil
}

// Creates a file in the zip writer with name 'filename' and writes 'contents' to it.
func writeFileToZip(ps *stepParams, filename string, contents []byte) error {
	ps.m.Lock()
	defer ps.m.Unlock()

	wr, err := ps.w.Create(filename)
	if err != nil {
		return err
	}
	_, err = wr.Write(contents)
	if err != nil {
		return fmt.Errorf("couldn't save '%s': %w", filename, err)
	}
	return nil
}

// Runs a command and pipes its output to a new file in the zip writer.
func writeCommandOutputToZipLimit(
	ps *stepParams,
	filename string,
	outputLimitBytes int,
	command string,
	args ...string,
) error {
	ps.m.Lock()
	defer ps.m.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), ps.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)

	wr, err := ps.w.Create(filename)
	if err != nil {
		return err
	}

	if outputLimitBytes > 0 {
		wr = &limitedWriter{
			w:          wr,
			limitBytes: outputLimitBytes,
		}
	}

	cmd.Stdout = wr
	cmd.Stderr = wr

	err = cmd.Start()
	if err != nil {
		return err
	}

	err = cmd.Wait()
	if err != nil {
		if strings.Contains(err.Error(), "broken pipe") {
			log.Debugf(
				"Got '%v' while running '%s'. This is probably due to the"+
					" command's output exceeding its limit in bytes.",
				err,
				cmd,
			)
		} else {
			return fmt.Errorf("couldn't save '%s': %w", filename, err)
		}
	}
	return nil
}

// Runs a command and pipes its output to a new file in the zip writer.
func writeCommandOutputToZip(
	ps *stepParams, filename, command string, args ...string,
) error {
	return writeCommandOutputToZipLimit(ps, filename, -1, command, args...)
}

func NewBundleCommand(fs afero.Fs) *cobra.Command {
	var (
		configFile string

		brokers   []string
		user      string
		password  string
		mechanism string
		enableTLS bool
		certFile  string
		keyFile   string
		CAFile    string

		adminURL       string
		adminEnableTLS bool
		adminCertFile  string
		adminKeyFile   string
		adminCAFile    string

		logsSince     string
		logsUntil     string
		logsSizeLimit string

		timeout time.Duration
	)
	command := &cobra.Command{
		Use:   "bundle",
		Short: "Collect environment data and create a bundle file for the Vectorized support team to inspect.",
		Long: `'rpk debug bundle' collects environment data that can help debug and diagnose
issues with a redpanda cluster, a broker, or the machine it's running on. It
then bundles the collected data into a zip file.

The following are the data sources that are bundled in the compressed file:

 - Kafka metadata: Broker configs, topic configs, start/committed/end offsets,
   groups, group commits.

 - Data directory structure: A file describing the data directory's contents.

 - redpanda configuration: The redpanda configuration file (redpanda.yaml;
   SASL credentials are stripped).

 - /proc/cpuinfo: CPU information like make, core count, cache, frequency.

 - /proc/interrupts: IRQ distribution across CPU cores.

 - Resource usage data: CPU usage percentage, free memory available for the
   redpanda process.

 - Clock drift: The ntp clock delta (using pool.ntp.org as a reference) & round
   trip time.

 - Kernel logs: The kernel logs ring buffer (syslog).

 - Broker metrics: The local broker's Prometheus metrics, fetched through its
   admin API.

 - DNS: The DNS info as reported by 'dig', using the hosts in
   /etc/resolv.conf.

 - Disk usage: The disk usage for the data directory, as output by 'du'.

 - redpanda logs: The redpanda logs written to journald. If --logs-since or
   --logs-until are passed, then only the logs within the resulting time frame
   will be included.

 - Socket info: The active sockets data output by 'ss'.

 - Running process info: As reported by 'top'.

 - Virtual memory stats: As reported by 'vmstat'.

 - Network config: As reported by 'ip addr'.

 - lspci: List the PCI buses and the devices connected to them.

 - dmidecode: The DMI table contents. Only included if this command is run
   as root.
`,
		SilenceUsage: true,
		Run: func(cmd *cobra.Command, args []string) {
			p := config.ParamsFromCommand(cmd)
			cfg, err := p.Load(fs)
			out.MaybeDie(err, "unable to load config: %v", err)

			admin, err := admin.NewClient(fs, cfg)
			out.MaybeDie(err, "unable to initialize admin client: %v", err)

			cl, err := kafka.NewFranzClient(fs, p, cfg)
			out.MaybeDie(err, "unable to initialize kafka client: %v", err)
			defer cl.Close()

			logsLimit, err := units.FromHumanSize(logsSizeLimit)
			out.MaybeDie(err, "unable to parse --logs-size-limit: %v", err)

			err = executeBundle(fs, cfg, cl, admin, logsSince, logsUntil, int(logsLimit), timeout)
			out.MaybeDie(err, "unable to create bundle: %v", err)
		},
	}
	command.Flags().StringVar(
		&adminURL,
		"admin-url",
		"",
		"The address to the broker's admin API. Defaults to the one in the config file.",
	)
	command.Flags().DurationVar(
		&timeout,
		"timeout",
		10*time.Second,
		"How long to wait for child commands to execute (e.g. '30s', '1.5m')",
	)
	command.Flags().StringVar(
		&logsSince,
		"logs-since",
		"",
		fmt.Sprintf(`Include log entries on or newer than the specified date. %s`, timeHelpText),
	)
	command.Flags().StringVar(
		&logsUntil,
		"logs-until",
		"",
		fmt.Sprintf(`Include log entries on or older than the specified date. %s`, timeHelpText),
	)
	command.Flags().StringVar(
		&logsSizeLimit,
		"logs-size-limit",
		"100MiB",
		"Read the logs until the given size is reached. Multipliers are also supported, e.g. 3MB, 1GiB.",
	)

	common.AddKafkaFlags(
		command,
		&configFile,
		&user,
		&password,
		&mechanism,
		&enableTLS,
		&certFile,
		&keyFile,
		&CAFile,
		&brokers,
	)
	common.AddAdminAPITLSFlags(command,
		&adminEnableTLS,
		&adminCertFile,
		&adminKeyFile,
		&adminCAFile,
	)

	return command
}

func executeBundle(
	fs afero.Fs,
	conf *config.Config,
	cl *kgo.Client,
	admin *admin.AdminAPI,
	logsSince, logsUntil string,
	logsLimitBytes int,
	timeout time.Duration,
) error {
	mode := os.FileMode(0755)
	timestamp := time.Now().Unix()
	filename := fmt.Sprintf("%d-bundle.zip", timestamp)
	f, err := fs.OpenFile(
		filename,
		os.O_CREATE|os.O_WRONLY,
		mode,
	)
	if err != nil {
		return fmt.Errorf("couldn't create bundle file: %w", err)
	}
	defer f.Close()

	grp := multierror.Group{}

	w := zip.NewWriter(f)
	defer w.Close()

	ps := &stepParams{
		fs:      fs,
		w:       w,
		timeout: timeout,
	}

	steps := []step{
		saveKafkaMetadata(ps, cl),
		saveDataDirStructure(ps, conf),
		saveConfig(ps, conf),
		saveCPUInfo(ps),
		saveInterrupts(ps),
		saveResourceUsageData(ps, conf),
		saveNTPDrift(ps),
		saveSyslog(ps),
		savePrometheusMetrics(ps, admin),
		saveDNSData(ps),
		saveDiskUsage(ps, conf),
		saveLogs(ps, logsSince, logsUntil, logsLimitBytes),
		saveSocketData(ps),
		saveTopOutput(ps),
		saveVmstat(ps),
		saveIp(ps),
		saveLspci(ps),
		saveDmidecode(ps),
	}

	for _, s := range steps {
		grp.Go(s)
	}

	errs := grp.Wait()
	if errs != nil {
		err := writeFileToZip(ps, "errors.txt", []byte(errs.Error()))
		if err != nil {
			errs = multierror.Append(errs, err)
		}
		log.Info(errs.Error())
	}

	log.Infof("Debug bundle saved to '%s'", filename)
	return nil
}

// Parses an error return from kadm, and if the return is a shard errors,
// returns a list of each individual error.
func stringifyKadmErr(err error) []string {
	var ae *kadm.AuthError
	var se *kadm.ShardErrors
	switch {
	case err == nil:
		return nil

	case errors.As(err, &se):
		var errs []string
		for _, err := range se.Errs {
			errs = append(errs, fmt.Sprintf("%s to %s (%d) failed: %s",
				se.Name,
				net.JoinHostPort(err.Broker.Host, strconv.Itoa(int(err.Broker.Port))),
				err.Broker.NodeID,
				err.Err,
			))
		}
		return errs

	case errors.As(err, &ae):
		return []string{fmt.Sprintf("authorization error: %s", err)}

	default:
		return []string{err.Error()}
	}
}

func saveKafkaMetadata(ps *stepParams, cl *kgo.Client) step {
	return func() error {
		log.Debug("Reading Kafka information")

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		type resp struct {
			Name     string      // the request the response is for
			Response interface{} // a raw response from kadm
			Error    []string    // no error, or one error, or potentially many shard errors
		}
		var resps []resp

		adm := kadm.NewClient(cl)

		meta, err := adm.Metadata(ctx)
		resps = append(resps, resp{
			Name:     "metadata",
			Response: meta,
			Error:    stringifyKadmErr(err),
		})

		tcs, err := adm.DescribeTopicConfigs(ctx, meta.Topics.Names()...)
		resps = append(resps, resp{
			Name:     "topic_configs",
			Response: tcs,
			Error:    stringifyKadmErr(err),
		})

		bcs, err := adm.DescribeBrokerConfigs(ctx, meta.Brokers.NodeIDs()...)
		resps = append(resps, resp{
			Name:     "broker_configs",
			Response: bcs,
			Error:    stringifyKadmErr(err),
		})

		ostart, err := adm.ListStartOffsets(ctx)
		resps = append(resps, resp{
			Name:     "log_start_offsets",
			Response: ostart,
			Error:    stringifyKadmErr(err),
		})

		ocommitted, err := adm.ListCommittedOffsets(ctx)
		resps = append(resps, resp{
			Name:     "last_stable_offsets",
			Response: ocommitted,
			Error:    stringifyKadmErr(err),
		})

		oend, err := adm.ListEndOffsets(ctx)
		resps = append(resps, resp{
			Name:     "high_watermarks",
			Response: oend,
			Error:    stringifyKadmErr(err),
		})

		groups, err := adm.DescribeGroups(ctx)
		resps = append(resps, resp{
			Name:     "groups",
			Response: groups,
			Error:    stringifyKadmErr(err),
		})

		fetched := adm.FetchManyOffsets(ctx, groups.Names()...)
		for _, fetch := range fetched {
			resps = append(resps, resp{
				Name:     fmt.Sprintf("group_commits_%s", fetch.Group),
				Response: fetch.Fetched,
				Error:    stringifyKadmErr(fetch.Err),
			})
		}

		marshal, err := json.Marshal(resps)
		if err != nil {
			return fmt.Errorf("unable to encode kafka admin responses: %v", err)
		}

		return writeFileToZip(ps, "kafka.json", marshal)
	}
}

// Walks the redpanda data directory recursively, and saves to the bundle
// a JSON map where the keys are the file/ dir paths, and the values are
// objects containing their data: size, mode, the file or dir it points to
// if the current file is a symlink, the time it was modified, its owner and
// its group, as well as an error message if reading that specific file failed.
func saveDataDirStructure(ps *stepParams, conf *config.Config) step {
	return func() error {
		files := make(map[string]*fileInfo)
		err := walkDir(conf.Redpanda.Directory, files)
		if err != nil {
			return fmt.Errorf("couldn't walk '%s': %w", conf.Redpanda.Directory, err)
		}
		bs, err := json.Marshal(files)
		if err != nil {
			return fmt.Errorf(
				"couldn't encode the '%s' directory structure as JSON: %w",
				conf.Redpanda.Directory,
				err,
			)
		}
		return writeFileToZip(ps, "data-dir.txt", bs)
	}
}

// Writes the config file to the bundle, redacting SASL credentials.
func saveConfig(ps *stepParams, conf *config.Config) step {
	return func() error {
		// Redact SASL credentials
		redacted := "(REDACTED)"
		if conf.Rpk.KafkaApi.SASL != nil {
			conf.Rpk.KafkaApi.SASL.User = redacted
			conf.Rpk.KafkaApi.SASL.Password = redacted
		}
		if conf.Rpk.SASL != nil {
			conf.Rpk.SASL.User = redacted
			conf.Rpk.SASL.Password = redacted
		}
		bs, err := yaml.Marshal(conf)
		if err != nil {
			return fmt.Errorf("couldn't encode the redpanda config as YAML: %w", err)
		}
		return writeFileToZip(ps, "redpanda.yaml", bs)
	}
}

// Saves the contents of /proc/cpuinfo
func saveCPUInfo(ps *stepParams) step {
	return func() error {
		bs, err := afero.ReadFile(ps.fs, "/proc/cpuinfo")
		if err != nil {
			return err
		}
		return writeFileToZip(ps, "proc/cpuinfo", bs)
	}
}

// Saves the contents of /proc/interrupts
func saveInterrupts(ps *stepParams) step {
	return func() error {
		bs, err := afero.ReadFile(ps.fs, "/proc/interrupts")
		if err != nil {
			return err
		}
		return writeFileToZip(ps, "proc/interrupts", bs)
	}
}

// Writes a file containing memory, disk & CPU usage metrics for a local
// redpanda process.
func saveResourceUsageData(ps *stepParams, conf *config.Config) step {
	return func() error {
		res, err := system.GatherMetrics(ps.fs, ps.timeout, *conf)
		if system.IsErrRedpandaDown(err) {
			return fmt.Errorf("omitting resource usage metrics: %w", err)
		}
		if err != nil {
			return fmt.Errorf("error gathering resource usage metrics: %w", err)
		}
		bs, err := json.Marshal(res)
		if err != nil {
			return fmt.Errorf("couldn't encode resource usage metrics: %w", err)
		}
		return writeFileToZip(ps, "resource-usage.json", bs)
	}
}

// Queries 'pool.ntp.org' and writes a file with the reported RTT, time & precision.
func saveNTPDrift(ps *stepParams) step {
	return func() error {
		const host = "pool.ntp.org"

		response, err := ntp.Query(host)
		if err != nil {
			return fmt.Errorf("error querying '%s': %w", host, err)
		}

		result := struct {
			Host            string    `json:"host"`
			RoundTripTimeMs int64     `json:"roundTripTimeMs"`
			Time            time.Time `json:"time"`
			PrecisionMs     int64     `json:"precisionMs"`
		}{
			Host:            host,
			RoundTripTimeMs: response.RTT.Milliseconds(),
			Time:            response.Time,
			PrecisionMs:     response.Precision.Milliseconds(),
		}

		marshalled, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("couldn't marshal the NTP response: %w", err)
		}

		return writeFileToZip(
			ps,
			"ntp.txt",
			marshalled,
		)
	}
}

func saveSyslog(ps *stepParams) step {
	return func() error {
		entries, err := syslog.ReadAll()
		if err != nil {
			return err
		}
		return writeFileToZip(ps, "syslog.txt", entries)
	}
}

// Queries the given admin API address for prometheus metrics.
func savePrometheusMetrics(ps *stepParams, admin *admin.AdminAPI) step {
	return func() error {
		raw, err := admin.PrometheusMetrics()
		if err != nil {
			return fmt.Errorf("unable to fetch metrics from the admin API: %w", err)
		}
		return writeFileToZip(ps, "prometheus-metrics.txt", raw)
	}
}

// Saves the output of `dig`
func saveDNSData(ps *stepParams) step {
	return func() error {
		return writeCommandOutputToZip(ps, "dig.txt", "dig")
	}
}

// Saves the disk usage total within redpanda's data directory.
func saveDiskUsage(ps *stepParams, conf *config.Config) step {
	return func() error {
		return writeCommandOutputToZip(
			ps,
			"du.txt",
			"du", "-h", conf.Redpanda.Directory,
		)
	}
}

// TODO: What if running inside a container/ k8s?
// Writes the journald redpanda logs, if available, to the bundle.
func saveLogs(ps *stepParams, since, until string, logsLimitBytes int) step {
	return func() error {
		args := []string{"--no-pager", "-u", "redpanda"}
		if since != "" {
			args = append(args, "--since", since)
		}
		if until != "" {
			args = append(args, "--until", until)
		}
		return writeCommandOutputToZipLimit(
			ps,
			"redpanda.log",
			logsLimitBytes,
			"journalctl",
			args...,
		)
	}
}

// Saves the output of `ss`
func saveSocketData(ps *stepParams) step {
	return func() error {
		return writeCommandOutputToZip(ps, "ss.txt", "ss")
	}
}

// Saves the output of `top`
func saveTopOutput(ps *stepParams) step {
	return func() error {
		return writeCommandOutputToZip(
			ps,
			"top.txt",
			"top", "-n", "10", "-H",
		)
	}
}

// Saves the output of `vmstat`
func saveVmstat(ps *stepParams) step {
	return func() error {
		return writeCommandOutputToZip(
			ps,
			"vmstat.txt",
			"vmstat", "-w", "1", "10",
		)
	}
}

// Saves the output of `ip addr`
func saveIp(ps *stepParams) step {
	return func() error {
		return writeCommandOutputToZip(
			ps,
			"ip.txt",
			"ip", "addr",
		)
	}
}

// Saves the output of `lspci`
func saveLspci(ps *stepParams) step {
	return func() error {
		return writeCommandOutputToZip(
			ps,
			"lspci.txt",
			"lspci",
		)
	}
}

// Saves the output of `dmidecode`
func saveDmidecode(ps *stepParams) step {
	return func() error {
		return writeCommandOutputToZip(
			ps,
			"dmidecode.txt",
			"dmidecode",
		)
	}
}

func walkDir(root string, files map[string]*fileInfo) error {
	return filepath.WalkDir(
		root,
		func(path string, d fs.DirEntry, readErr error) error {
			// Prevent infinite loops.
			if _, exists := files[path]; exists {
				return nil
			}

			i := new(fileInfo)
			files[path] = i

			info, err := d.Info()
			if err != nil {
				i.Error = err.Error()
				// If reading a directory failed, then skip it altogether.
				if d.IsDir() {
					return fs.SkipDir
				}
				// If it's just a file, just return and move to the
				// next entry.
				return nil
			}

			i.Size = units.HumanSize(float64(info.Size()))
			i.Mode = info.Mode().String()
			i.Modified = info.ModTime().String()

			// The user and group are only available through the
			// underlying syscall object.
			sys, ok := info.Sys().(*syscall.Stat_t)
			if ok {
				u, err := user.LookupId(fmt.Sprint(sys.Uid))
				if err == nil {
					i.User = u.Name
				} else {
					i.User = fmt.Sprintf("user lookup failed for UID %d: %v", sys.Uid, err)
				}
				g, err := user.LookupGroupId(fmt.Sprint(sys.Gid))
				if err == nil {
					i.Group = g.Name
				} else {
					i.Group = fmt.Sprintf("group lookup failed for GID %d: %v", sys.Gid, err)
				}
			}
			// If the directory's contents couldn't be read, skip it.
			if readErr != nil {
				i.Error = readErr.Error()
				if d.IsDir() {
					return fs.SkipDir
				}
			}

			// If it's a symlink, save the dir or file it points to.
			// If the file it points to is a directory, follow it and then
			// call `walk` using it as the root.
			isSymlink := info.Mode().Type()&fs.ModeSymlink != 0
			if !isSymlink {
				return nil
			}

			dest, err := os.Readlink(path)
			if err != nil {
				i.Symlink = "unresolvable"
				i.Error = err.Error()
			}
			i.Symlink = dest

			fInfo, err := os.Stat(dest)
			if err != nil {
				files[dest] = &fileInfo{Error: err.Error()}
			} else if fInfo.IsDir() {
				return walkDir(dest, files)
			}

			return nil
		},
	)
}
