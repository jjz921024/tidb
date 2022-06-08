package task

import (
	"bytes"
	"context"
	"github.com/pingcap/errors"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/br/pkg/glue"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/summary"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/tikv/client-go/v2/txnkv"
	"go.uber.org/zap"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	backupFileName = "data_"
	defaultPrefix  = "all"

	flagFileSize = "size"
	flagPrefix   = "prefix"
)

const (
	CompressionType_NONE = iota
	CompressionType_GZIP
)

type TxnKvConfig struct {
	Config

	Prefix      string `json:"prefix" toml:"prefix"`
	FileSize    int    `json:"size" toml:"size"`
	Compression string `json:"compression" toml:"compression"`
}

func DefineSnapshotBackupFlags(command *cobra.Command) {
	command.Flags().String(flagPrefix, defaultPrefix, "backup special prefix data")
	command.Flags().Int(flagFileSize, 100*1024*1024, "the size of each backup file")
	command.Flags().String(flagCompressionType, "none",
		"backup file compression algorithm, value can be one of 'none|gzip'")
}

func (cfg *TxnKvConfig) ParseBackupConfigFromFlags(flags *pflag.FlagSet) error {
	var err error
	cfg.Prefix, err = flags.GetString(flagPrefix)
	if err != nil {
		return errors.Trace(err)
	}
	cfg.FileSize, err = flags.GetInt(flagFileSize)
	if err != nil {
		return errors.Trace(err)
	}
	cfg.Compression, err = flags.GetString(flagCompressionType)
	if err != nil {
		return errors.Trace(err)
	}
	if err = cfg.Config.ParseFromFlags(flags); err != nil {
		return errors.Trace(err)
	}
	return nil
}

func RunBackupTxn(c context.Context, g glue.Glue, cmdName string, cfg *TxnKvConfig) error {
	cfg.adjust()

	defer summary.Summary(cmdName)
	ctx, cancel := context.WithCancel(c)
	defer cancel()

	// 创建连接和快照
	cli, err := txnkv.NewClient(cfg.PD)
	if err != nil {
		return errors.Trace(err)
	}
	txn, err := cli.Begin()
	if err != nil {
		return errors.Trace(err)
	}

	snapshot := txn.GetSnapshot()

	// 过滤指定前缀的key
	startKey, endKey := []byte(""), []byte("")
	if cfg.Prefix != defaultPrefix {
		startKey = []byte(cfg.Prefix)
		k := make([]byte, len(startKey))
		copy(k, startKey)
		k[len(k)-1] += 1
		endKey = k
	}
	// 包头不包尾
	iter, err := snapshot.Iter(startKey, endKey)
	if err != nil {
		return errors.Trace(err)
	}

	// 解析后端存储, 暂时只支持local
	u, err := storage.ParseBackend(cfg.Storage, &cfg.BackendOptions)
	if err != nil {
		return errors.Trace(err)
	}
	if _, ok := u.Backend.(*backuppb.StorageBackend_Local); !ok {
		return errors.New("only support local storage when backup txn")
	}

	// 创建本地存储目录
	l := u.GetLocal()
	id := cli.GetClusterID()
	ts := time.Now().Format("20060102150405")
	path := mkdirBackupPath(l.Path, id, cfg.Prefix, ts)
	l.Path = path

	// 创建对应存储的writer
	opts := storage.ExternalStorageOptions{
		NoCredentials:   cfg.NoCreds,
		SendCredentials: cfg.SendCreds,
	}
	s, err := createStorage(ctx, u, &opts)
	if err != nil {
		return errors.Trace(err)
	}

	// 暂时只支持gzip
	if cfg.Compression != "none" {
		s = storage.WithCompression(s, CompressionType_GZIP)
	}

	log.Info("start backup txn kv",
		zap.String("path", path),
		zap.String("prefix", cfg.Prefix),
		zap.Int("size", cfg.FileSize),
		zap.String("compression", cfg.Compression),
	)

	size := 0
	idx := 0
	var writer storage.ExternalFileWriter
	defer func() {
		if writer != nil {
			_ = writer.Close(ctx)
		}
	}()

	var buf bytes.Buffer
	for iter.Valid() {
		buf.Write(iter.Key())
		buf.WriteByte('\t')
		buf.Write(iter.Value())
		buf.WriteByte('\n')

		if writer == nil || size >= cfg.FileSize {
			if writer != nil {
				size = 0
				idx += 1
				_ = writer.Close(ctx)
			}
			f := backupFileName + strconv.Itoa(idx)

			writer, _ = s.Create(ctx, f)
			log.Info("create dump file", zap.String("filename", f))
		}

		size += buf.Len()
		_, _ = writer.Write(ctx, buf.Bytes())
		buf.Reset()

		err = iter.Next()
		if err != nil {
			return errors.Trace(err)
		}
	}

	// Set task summary to success status.
	summary.SetSuccessStatus(true)
	return nil
}

// 在指定backup目录下创建此时backup的子目录
// 目录名: <clusterId>_<prefix>_<timestamp>
func mkdirBackupPath(p string, clusterId uint64, prefix string, ts string) string {
	_, err := os.Stat(p)
	if err != nil {
		_ = os.MkdirAll(p, 644)
	}

	subPath := strconv.FormatUint(clusterId, 10) + "_" + prefix + "_" + ts
	join := filepath.Join(p, subPath)
	_ = os.Mkdir(join, 644)

	return join
}

func createStorage(ctx context.Context, backend *backuppb.StorageBackend,
	opts *storage.ExternalStorageOptions) (storage.ExternalStorage, error) {
	var err error
	s, err := storage.New(ctx, backend, opts)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return s, nil
}
