// +build !nodaemon

package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"runtime/pprof"
	"strings"

	paramfetch "github.com/filecoin-project/go-paramfetch"
	metricsprom "github.com/ipfs/go-metrics-prometheus"
	"github.com/mitchellh/go-homedir"
	"github.com/multiformats/go-multiaddr"
	"github.com/urfave/cli/v2"
	"go.opencensus.io/plugin/runmetrics"
	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"golang.org/x/xerrors"
	"gopkg.in/cheggaaa/pb.v1"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/stmgr"
	"github.com/filecoin-project/lotus/chain/store"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/vm"
	lcli "github.com/filecoin-project/lotus/cli"
	"github.com/filecoin-project/lotus/extern/sector-storage/ffiwrapper"
	"github.com/filecoin-project/lotus/journal"
	"github.com/filecoin-project/lotus/lib/peermgr"
	"github.com/filecoin-project/lotus/lib/ulimit"
	"github.com/filecoin-project/lotus/metrics"
	"github.com/filecoin-project/lotus/node"
	"github.com/filecoin-project/lotus/node/modules"
	"github.com/filecoin-project/lotus/node/modules/dtypes"
	"github.com/filecoin-project/lotus/node/modules/testing"
	"github.com/filecoin-project/lotus/node/repo"
)

const (
	makeGenFlag     = "lotus-make-genesis"
	preTemplateFlag = "genesis-template"
)

var daemonStopCmd = &cli.Command{
	Name:  "stop",
	Usage: "Stop a running lotus daemon",
	Flags: []cli.Flag{},
	Action: func(cctx *cli.Context) error {
		api, closer, err := lcli.GetAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()

		err = api.Shutdown(lcli.ReqContext(cctx))
		if err != nil {
			return err
		}

		return nil
	},
}

// DaemonCmd is the `go-lotus daemon` command
var DaemonCmd = &cli.Command{
	Name:  "daemon",
	Usage: "Start a lotus daemon process",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "api",
			Value: "1234",
		},
		&cli.StringFlag{
			Name:   makeGenFlag,
			Value:  "",
			Hidden: true,
		},
		&cli.StringFlag{
			Name:   preTemplateFlag,
			Hidden: true,
		},
		&cli.StringFlag{
			Name:   "import-key",
			Usage:  "on first run, import a default key from a given file",
			Hidden: true,
		},
		&cli.StringFlag{
			Name:  "genesis",
			Usage: "genesis file to use for first node run",
		},
		&cli.BoolFlag{
			Name:  "bootstrap",
			Value: true,
		},
		&cli.StringFlag{
			Name:  "import-chain",
			Usage: "on first run, load chain from given file or url and validate",
		},
		&cli.StringFlag{
			Name:  "import-snapshot",
			Usage: "import chain state from a given chain export file or url",
		},
		&cli.BoolFlag{
			Name:  "halt-after-import",
			Usage: "halt the process after importing chain from file",
		},
		&cli.BoolFlag{
			Name:   "lite",
			Usage:  "start lotus in lite mode",
			Hidden: true,
		},
		&cli.StringFlag{
			Name:  "pprof",
			Usage: "specify name of file for writing cpu profile to",
		},
		&cli.StringFlag{
			Name:  "profile",
			Usage: "specify type of node",
		},
		&cli.BoolFlag{
			Name:  "manage-fdlimit",
			Usage: "manage open file limit",
			Value: true,
		},
		&cli.StringFlag{
			Name:  "config",
			Usage: "specify path of config file to use",
		},
		// FIXME: This is not the correct place to put this configuration
		//  option. Ideally it would be part of `config.toml` but at the
		//  moment that only applies to the node configuration and not outside
		//  components like the RPC server.
		&cli.IntFlag{
			Name:  "api-max-req-size",
			Usage: "maximum API request size accepted by the JSON RPC server",
		},
	},
	Action: func(cctx *cli.Context) error {
		isLite := cctx.Bool("lite")

		// ADDBYGABE
		// 启用收集CUP和内存指标数据
		err := runmetrics.Enable(runmetrics.RunMetricOptions{
			EnableCPU:    true,
			EnableMemory: true,
		})
		if err != nil {
			return xerrors.Errorf("enabling runtime metrics: %w", err)
		}

		// ADDBYGABE
		// 设置最大文件描述符？
		if cctx.Bool("manage-fdlimit") {
			if _, _, err := ulimit.ManageFdLimit(); err != nil {
				log.Errorf("setting file descriptor limit: %s", err)
			}
		}

		// ADDBYGABE
		// 启用当前进程的CPU性能分析
		if prof := cctx.String("pprof"); prof != "" {
			profile, err := os.Create(prof)
			if err != nil {
				return err
			}

			if err := pprof.StartCPUProfile(profile); err != nil {
				return err
			}
			defer pprof.StopCPUProfile()
		}

		// ADDBYGABE
		// 指定node的类型？
		var isBootstrapper dtypes.Bootstrapper
		switch profile := cctx.String("profile"); profile {
		case "bootstrapper":
			isBootstrapper = true
		case "":
			// do nothing
		default:
			return fmt.Errorf("unrecognized profile type: %q", profile)
		}

		// ADDBYGABE
		// 新建ctx实例，入参包含：空ctx，版本号，提交的commit
		ctx, _ := tag.New(context.Background(), tag.Insert(metrics.Version, build.BuildVersion), tag.Insert(metrics.Commit, build.CurrentCommit))
		{
			dir, err := homedir.Expand(cctx.String("repo"))
			if err != nil {
				log.Warnw("could not expand repo location", "error", err)
			} else {
				log.Infof("lotus repo: %s", dir)
			}
		}

		// ADDBYGABE
		// r结构体实例包含lotus主目录地址，和配置文件地址
		r, err := repo.NewFS(cctx.String("repo"))
		if err != nil {
			return xerrors.Errorf("opening fs repo: %w", err)
		}

		// ADDBYGABE
		// 如果设置了配置文件参数，覆盖r中的配置文件地址
		if cctx.String("config") != "" {
			r.SetConfigPath(cctx.String("config"))
		}

		// ADDBYGABE
		// 创建主目录及配置文件，并且写入(默认)配置内容到配置文件
		if err := r.Init(repo.FullNode); err != nil && err != repo.ErrRepoExists {
			return xerrors.Errorf("repo init error: %w", err)
		}

		// ADDBYGABE
		// 非精简模式，需要下载证明参数文件
		// 默认保存位置"/var/tmp/filecoin-proof-parameters"
		if !isLite {
			if err := paramfetch.GetParams(lcli.ReqContext(cctx), build.ParametersJSON(), 0); err != nil {
				return xerrors.Errorf("fetching proof parameters: %w", err)
			}
		}

		// ADDBYGABE
		// 用于第一个节点的创世文件
		// 没有指定参数，则利用rice.FindBox在当前目录生成默认的“devnet.car”文件
		var genBytes []byte
		if cctx.String("genesis") != "" {
			genBytes, err = ioutil.ReadFile(cctx.String("genesis"))
			if err != nil {
				return xerrors.Errorf("reading genesis: %w", err)
			}
		} else {
			genBytes = build.MaybeGenesis()
		}

		// ADDBYGABE
		// 导入链文件或快照，可以是文件或url
		// 这两个参数不能同时给定
		// 有halt-after-import参数，导入结束后，命令函数也结束
		chainfile := cctx.String("import-chain")
		snapshot := cctx.String("import-snapshot")
		if chainfile != "" || snapshot != "" {
			if chainfile != "" && snapshot != "" {
				return fmt.Errorf("cannot specify both 'import-snapshot' and 'import-chain'")
			}
			var issnapshot bool
			if chainfile == "" {
				chainfile = snapshot
				issnapshot = true
			}

			if err := ImportChain(r, chainfile, issnapshot); err != nil {
				return err
			}
			if cctx.Bool("halt-after-import") {
				fmt.Println("Chain import complete, halting as requested...")
				return nil
			}
		}

		// ADDBYGABE
		// genesis是一个闭包函数
		genesis := node.Options()
		if len(genBytes) > 0 {
			genesis = node.Override(new(modules.Genesis), modules.LoadGenesis(genBytes))
		}
		// ADDBYGABE
		// 创世文件参数：makeGenFlag = "lotus-make-genesis"
		// 创世模板文件参数：preTemplateFlag = "genesis-template"
		// testing.MakeGenesis 为创建创世区块函数
		if cctx.String(makeGenFlag) != "" {
			if cctx.String(preTemplateFlag) == "" {
				return xerrors.Errorf("must also pass file with genesis template to `--%s`", preTemplateFlag)
			}
			genesis = node.Override(new(modules.Genesis), testing.MakeGenesis(cctx.String(makeGenFlag), cctx.String(preTemplateFlag)))
		}

		// ADDBYGABE
		// 关闭信号
		shutdownChan := make(chan struct{})

		// If the daemon is started in "lite mode", provide a  GatewayAPI
		// for RPC calls
		// ADDBYGABE
		// 如果精简模式，提供一个网关API服务给RPC调用
		liteModeDeps := node.Options()
		if isLite {
			gapi, closer, err := lcli.GetGatewayAPI(cctx)
			if err != nil {
				return err
			}

			defer closer()
			liteModeDeps = node.Override(new(api.GatewayAPI), gapi)
		}

		// some libraries like ipfs/go-ds-measure and ipfs/go-ipfs-blockstore
		// use ipfs/go-metrics-interface. This injects a Prometheus exporter
		// for those. Metrics are exported to the default registry.
		// ADDBYGABE
		// ipfs/go-ds-measure and ipfs/go-ipfs-blockstore 使用了 ipfs/go-metrics-interface
		// ipfs/go-metrics-interface 给它们注入了 Prometheus exporter
		// 指标导出到默认注册表？
		if err := metricsprom.Inject(); err != nil {
			log.Warnf("unable to inject prometheus ipfs/go-metrics exporter; some metrics will be unavailable; err: %s", err)
		}

		// ADDBYGABE
		// FullNode 是一个底层接口，需要实现非常多的方法
		var api api.FullNode
		// ADDBYGABE
		// 创建或开始一个新的filecoin node
		stop, err := node.New(ctx,
			node.FullAPI(&api, node.Lite(isLite)),

			node.Override(new(dtypes.Bootstrapper), isBootstrapper),
			node.Override(new(dtypes.ShutdownChan), shutdownChan),
			node.Online(),
			node.Repo(r),

			genesis,
			liteModeDeps,

			// ADDBYGABE
			// 这里的api其实就是端口号
			// 如果配置文件里面没有设置，才会从命令行参数中获取
			node.ApplyIf(func(s *node.Settings) bool { return cctx.IsSet("api") },
				node.Override(node.SetApiEndpointKey, func(lr repo.LockedRepo) error {
					apima, err := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/" +
						cctx.String("api"))
					if err != nil {
						return err
					}
					return lr.SetAPIEndpoint(apima)
				})),
			node.ApplyIf(func(s *node.Settings) bool { return !cctx.Bool("bootstrap") },
				node.Unset(node.RunPeerMgrKey),
				node.Unset(new(*peermgr.PeerMgr)),
			),
		)
		if err != nil {
			return xerrors.Errorf("initializing node: %w", err)
		}

		// ADDBYGABE
		// 默认钱包的key
		// import-key 是一个相对于homedir的文件路径名
		if cctx.String("import-key") != "" {
			if err := importKey(ctx, api, cctx.String("import-key")); err != nil {
				log.Errorf("importing key failed: %+v", err)
			}
		}

		// Register all metric views
		if err = view.Register(
			metrics.DefaultViews...,
		); err != nil {
			log.Fatalf("Cannot register the view: %v", err)
		}

		// Set the metric to one so it is published to the exporter
		stats.Record(ctx, metrics.LotusInfo.M(1))

		// ADDBYGABE
		// endpoint 是一个包含[]byte的结构体，实现了许多工具方法
		endpoint, err := r.APIEndpoint()
		if err != nil {
			return xerrors.Errorf("getting api endpoint: %w", err)
		}

		// TODO: properly parse api endpoint (or make it a URL)
		// ADDBYGABE
		// 启动RPC服务
		return serveRPC(api, stop, endpoint, shutdownChan, int64(cctx.Int("api-max-req-size")))
	},
	Subcommands: []*cli.Command{
		daemonStopCmd,
	},
}

func importKey(ctx context.Context, api api.FullNode, f string) error {
	f, err := homedir.Expand(f)
	if err != nil {
		return err
	}

	hexdata, err := ioutil.ReadFile(f)
	if err != nil {
		return err
	}

	data, err := hex.DecodeString(strings.TrimSpace(string(hexdata)))
	if err != nil {
		return err
	}

	var ki types.KeyInfo
	if err := json.Unmarshal(data, &ki); err != nil {
		return err
	}

	addr, err := api.WalletImport(ctx, &ki)
	if err != nil {
		return err
	}

	if err := api.WalletSetDefault(ctx, addr); err != nil {
		return err
	}

	log.Infof("successfully imported key for %s", addr)
	return nil
}

func ImportChain(r repo.Repo, fname string, snapshot bool) (err error) {
	var rd io.Reader
	var l int64
	if strings.HasPrefix(fname, "http://") || strings.HasPrefix(fname, "https://") {
		resp, err := http.Get(fname) //nolint:gosec
		if err != nil {
			return err
		}
		defer resp.Body.Close() //nolint:errcheck

		if resp.StatusCode != http.StatusOK {
			return xerrors.Errorf("non-200 response: %d", resp.StatusCode)
		}

		rd = resp.Body
		l = resp.ContentLength
	} else {
		fname, err = homedir.Expand(fname)
		if err != nil {
			return err
		}

		fi, err := os.Open(fname)
		if err != nil {
			return err
		}
		defer fi.Close() //nolint:errcheck

		st, err := os.Stat(fname)
		if err != nil {
			return err
		}

		rd = fi
		l = st.Size()
	}

	lr, err := r.Lock(repo.FullNode)
	if err != nil {
		return err
	}
	defer lr.Close() //nolint:errcheck

	bs, err := lr.Blockstore(repo.BlockstoreChain)
	if err != nil {
		return xerrors.Errorf("failed to open blockstore: %w", err)
	}

	mds, err := lr.Datastore("/metadata")
	if err != nil {
		return err
	}

	j, err := journal.OpenFSJournal(lr, journal.EnvDisabledEvents())
	if err != nil {
		return xerrors.Errorf("failed to open journal: %w", err)
	}

	cst := store.NewChainStore(bs, bs, mds, vm.Syscalls(ffiwrapper.ProofVerifier), j)
	defer cst.Close() //nolint:errcheck

	log.Infof("importing chain from %s...", fname)

	bufr := bufio.NewReaderSize(rd, 1<<20)

	bar := pb.New64(l)
	br := bar.NewProxyReader(bufr)
	bar.ShowTimeLeft = true
	bar.ShowPercent = true
	bar.ShowSpeed = true
	bar.Units = pb.U_BYTES

	bar.Start()
	ts, err := cst.Import(br)
	bar.Finish()

	if err != nil {
		return xerrors.Errorf("importing chain failed: %w", err)
	}

	if err := cst.FlushValidationCache(); err != nil {
		return xerrors.Errorf("flushing validation cache failed: %w", err)
	}

	gb, err := cst.GetTipsetByHeight(context.TODO(), 0, ts, true)
	if err != nil {
		return err
	}

	err = cst.SetGenesis(gb.Blocks()[0])
	if err != nil {
		return err
	}

	stm := stmgr.NewStateManager(cst)

	if !snapshot {
		log.Infof("validating imported chain...")
		if err := stm.ValidateChain(context.TODO(), ts); err != nil {
			return xerrors.Errorf("chain validation failed: %w", err)
		}
	}

	log.Infof("accepting %s as new head", ts.Cids())
	if err := cst.ForceHeadSilent(context.Background(), ts); err != nil {
		return err
	}

	return nil
}
