package commands

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hyperledger/burrow/config/source"
	"github.com/hyperledger/burrow/crypto"
	"github.com/hyperledger/burrow/execution/evm/abi"
	"github.com/hyperledger/burrow/logging/logconfig"
	"github.com/hyperledger/burrow/vent/config"
	"github.com/hyperledger/burrow/vent/service"
	"github.com/hyperledger/burrow/vent/sqldb"
	"github.com/hyperledger/burrow/vent/sqlsol"
	"github.com/hyperledger/burrow/vent/types"
	cli "github.com/jawher/mow.cli"
)

type LogLevel string

const (
	LogLevelNone  LogLevel = "none"
	LogLevelInfo  LogLevel = "info"
	LogLevelTrace LogLevel = "trace"
)

func logConfig(level LogLevel) *logconfig.LoggingConfig {
	logConf := logconfig.New()
	switch level {
	case LogLevelNone:
		return logConf.None()
	case LogLevelTrace:
		return logConf.WithTrace()
	default:
		return logConf
	}
}

// Vent consumes EVM events and commits to a DB
func Vent(output Output) func(cmd *cli.Cmd) {
	return func(cmd *cli.Cmd) {

		cmd.Command("start", "Start the Vent consumer service",
			func(cmd *cli.Cmd) {
				cfg := config.DefaultVentConfig()

				dbOpts := sqlDBOpts(cmd, cfg)
				grpcAddrOpt := cmd.StringOpt("chain-addr", cfg.ChainAddress, "Address to connect to the Hyperledger Burrow gRPC server")
				httpAddrOpt := cmd.StringOpt("http-addr", cfg.HTTPListenAddress, "Address to bind the HTTP server")
				logLevelOpt := cmd.StringOpt("log-level", string(LogLevelInfo), "Logging level (none, info, trace)")
				watchAddressesOpt := cmd.StringsOpt("watch", nil, "Add contract address to global watch filter")
				minimumHeightOpt := cmd.IntOpt("minimum-height", 0, "Only process block greater than or equal to height passed")
				maxRetriesOpt := cmd.IntOpt("max-retries", int(cfg.BlockConsumerConfig.MaxRetries), "Maximum number of retries when consuming blocks")
				maxRequestRateOpt := cmd.StringOpt("max-request-rate", "", "Maximum request rate given as (number of requests)/(time base), e.g. 1000/24h for 1000 requests per day")
				backoffDurationOpt := cmd.StringOpt("backoff", "",
					"The minimum duration to wait before asking for new blocks - increases exponentially when errors occur. Values like 200ms, 1s, 2m")
				batchSizeOpt := cmd.IntOpt("batch-size", int(cfg.BlockConsumerConfig.MaxBlockBatchSize),
					"The maximum number of blocks from which to request events in a single call - will reduce logarithmically to 1 when errors occur.")
				abiFileOpt := cmd.StringsOpt("abi", cfg.AbiFileOrDirs, "EVM Contract ABI file or folder")
				specFileOrDirOpt := cmd.StringsOpt("spec", cfg.SpecFileOrDirs, "SQLSol specification file or folder")
				dbBlockOpt := cmd.BoolOpt("blocks", false, "Create block tables and persist related data")
				dbTxOpt := cmd.BoolOpt("txs", false, "Create tx tables and persist related data")

				announceEveryOpt := cmd.StringOpt("announce-every", "5s", "Announce vent status every period as a Go duration, e.g. 1ms, 3s, 1h")

				cmd.Before = func() {
					var err error
					// Rather annoying boilerplate here... but there is no way to pass mow.cli a pointer for it to fill you value
					cfg.DBAdapter = *dbOpts.adapter
					cfg.DBURL = *dbOpts.url
					cfg.DBSchema = *dbOpts.schema
					cfg.ChainAddress = *grpcAddrOpt
					cfg.HTTPListenAddress = *httpAddrOpt
					cfg.WatchAddresses = make([]crypto.Address, len(*watchAddressesOpt))
					cfg.MinimumHeight = uint64(*minimumHeightOpt)
					cfg.BlockConsumerConfig.MaxRequests, cfg.BlockConsumerConfig.TimeBase, err = parseRequestRate(*maxRequestRateOpt)
					if err != nil {
						output.Fatalf("Could not parse max request rate: %w", err)
					}
					cfg.BlockConsumerConfig.MaxRetries = uint64(*maxRetriesOpt)
					cfg.BlockConsumerConfig.BaseBackoffDuration, err = parseDuration(*backoffDurationOpt)
					if err != nil {
						output.Fatalf("could not parse backoff duration: %w", err)
					}
					cfg.BlockConsumerConfig.MaxBlockBatchSize = uint64(*batchSizeOpt)
					for i, wa := range *watchAddressesOpt {
						cfg.WatchAddresses[i], err = crypto.AddressFromHexString(wa)
						if err != nil {
							output.Fatalf("could not parse watch address: %w", err)
						}
					}
					cfg.AbiFileOrDirs = *abiFileOpt
					cfg.SpecFileOrDirs = *specFileOrDirOpt
					if *dbBlockOpt {
						cfg.SpecOpt |= sqlsol.Block
					}
					if *dbTxOpt {
						cfg.SpecOpt |= sqlsol.Tx
					}

					cfg.AnnounceEvery, err = parseDuration(*announceEveryOpt)
					if err != nil {
						output.Fatalf("could not parse announce-every duration %s: %v", *announceEveryOpt, err)
					}
				}

				cmd.Spec = "--spec=<spec file or dir>... [--abi=<abi file or dir>...] " +
					"[--watch=<contract address>...] [--minimum-height=<lowest height from which to read>] " +
					"[--max-retries=<max block request retries>] [--backoff=<minimum backoff duration>] " +
					"[--max-request-rate=<requests / time base>] [--batch-size=<minimum block batch size>] " +
					"[--db-adapter] [--db-url] [--db-schema] [--blocks] [--txs] [--chain-addr] [--http-addr] " +
					"[--log-level] [--announce-every=<duration>]"

				cmd.Action = func() {
					logger, err := logConfig(LogLevel(*logLevelOpt)).Logger()
					if err != nil {
						output.Fatalf("failed to load logger: %v", err)
					}

					logger = logger.With("service", "vent")
					consumer := service.NewConsumer(cfg, logger, make(chan types.EventData))
					if err != nil {
						output.Fatalf("Could not create Vent Consumer: %v", err)
					}
					server := service.NewServer(cfg, logger, consumer)

					projection, err := sqlsol.SpecLoader(cfg.SpecFileOrDirs, cfg.SpecOpt)
					if err != nil {
						output.Fatalf("Spec loader error: %v", err)
					}

					var wg sync.WaitGroup

					// setup channel for termination signals
					ch := make(chan os.Signal)

					signal.Notify(ch, syscall.SIGTERM)
					signal.Notify(ch, syscall.SIGINT)

					// start the events consumer
					wg.Add(1)

					go func() {
						if err := consumer.Run(projection, true); err != nil {
							output.Fatalf("Consumer execution error: %v", err)
						}

						wg.Done()
					}()

					// start the http server
					wg.Add(1)

					go func() {
						server.Run()
						wg.Done()
					}()

					// wait for a termination signal from the OS and
					// gracefully shutdown the events consumer and the http server
					go func() {
						<-ch
						consumer.Shutdown()
						server.Shutdown()
					}()

					// wait until the events consumer and the http server are done
					wg.Wait()
				}
			})

		cmd.Command("schema", "Print JSONSchema for spec file format to validate table specs",
			func(cmd *cli.Cmd) {
				cmd.Action = func() {
					output.Printf(source.JSONString(types.ProjectionSpecSchema()))
				}
			})

		cmd.Command("spec", "Generate SQLSOL specification from ABIs",
			func(cmd *cli.Cmd) {
				abiFileOpt := cmd.StringsOpt("abi", nil, "EVM Contract ABI file or folder")
				dest := cmd.StringArg("SPEC", "", "Write resulting spec to this json file")

				cmd.Action = func() {
					abiSpec, err := abi.LoadPath(*abiFileOpt...)
					if err != nil {
						output.Fatalf("ABI loader error: %v", err)
					}

					spec, err := sqlsol.GenerateSpecFromAbis(abiSpec)
					if err != nil {
						output.Fatalf("error generating spec: %s\n", err)
					}

					err = ioutil.WriteFile(*dest, []byte(source.JSONString(spec)), 0644)
					if err != nil {
						output.Fatalf("error writing file: %v\n", err)
					}
				}
			})

		cmd.Command("restore", "Restore the mapped tables from the _vent_log table",
			func(cmd *cli.Cmd) {
				const timeLayout = "2006-01-02 15:04:05"

				dbOpts := sqlDBOpts(cmd, config.DefaultVentConfig())
				timeOpt := cmd.StringOpt("t time", "", fmt.Sprintf("restore time up to which all "+
					"log entries will be applied to restore DB, in the format '%s'- restores all log entries if omitted",
					timeLayout))
				prefixOpt := cmd.StringOpt("p prefix", "", "")

				cmd.Spec = "[--db-adapter] [--db-url] [--db-schema] [--time=<date/time to up to which to restore>] " +
					"[--prefix=<destination table prefix>]"

				var restoreTime time.Time

				cmd.Before = func() {
					if *timeOpt != "" {
						var err error
						restoreTime, err = time.Parse(timeLayout, *timeOpt)
						if err != nil {
							output.Fatalf("Could not parse restore time, should be in the format '%s': %v",
								timeLayout, err)
						}
					}
				}

				cmd.Action = func() {
					log, err := logconfig.New().Logger()
					if err != nil {
						output.Fatalf("failed to load logger: %v", err)
					}
					db, err := sqldb.NewSQLDB(types.SQLConnection{
						DBAdapter: *dbOpts.adapter,
						DBURL:     *dbOpts.url,
						DBSchema:  *dbOpts.schema,
						Log:       log.With("service", "vent"),
					})
					if err != nil {
						output.Fatalf("Could not connect to SQL DB: %v", err)
					}

					if restoreTime.IsZero() {
						output.Logf("Restoring DB to state from log")
					} else {
						output.Logf("Restoring DB to state from log as of %v", restoreTime)
					}

					if *prefixOpt == "" {
						output.Logf("Restoring DB in-place by overwriting any existing tables")
					} else {
						output.Logf("Restoring DB to destination tables with prefix '%s'", *prefixOpt)
					}

					err = db.RestoreDB(restoreTime, *prefixOpt)
					if err != nil {
						output.Fatalf("Error restoring DB: %v", err)
					}
					output.Logf("Successfully restored DB")
				}
			})
	}
}

func parseDuration(duration string) (time.Duration, error) {
	if duration == "" {
		return 0, nil
	}
	return time.ParseDuration(duration)
}

func parseRequestRate(rate string) (int, time.Duration, error) {
	if rate == "" {
		return 0, 0, nil
	}
	ratio := strings.Split(rate, "/")
	if len(ratio) != 2 {
		return 0, 0, fmt.Errorf("expected a ratio string separated by a '/' but got %s", rate)
	}
	requests, err := strconv.ParseInt(ratio[0], 10, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("could not parse max requests as base 10 integer: %w", err)
	}
	timeBase, err := time.ParseDuration(ratio[1])
	if err != nil {
		return 0, 0, fmt.Errorf("could not parse time base: %w", err)
	}
	return int(requests), timeBase, nil
}

type dbOpts struct {
	adapter *string
	url     *string
	schema  *string
}

func sqlDBOpts(cmd *cli.Cmd, cfg *config.VentConfig) dbOpts {
	return dbOpts{
		adapter: cmd.StringOpt("db-adapter", cfg.DBAdapter, "Database adapter, 'postgres' or 'sqlite' (if built with the sqlite tag) are supported"),
		url:     cmd.StringOpt("db-url", cfg.DBURL, "PostgreSQL database URL or SQLite db file path"),
		schema:  cmd.StringOpt("db-schema", cfg.DBSchema, "PostgreSQL database schema (empty for SQLite)"),
	}
}
