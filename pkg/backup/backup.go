package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlexAkulov/clickhouse-backup/config"
	"github.com/AlexAkulov/clickhouse-backup/pkg/clickhouse"
	"github.com/AlexAkulov/clickhouse-backup/pkg/metadata"

	"github.com/apex/log"
)

const (
	// BackupTimeFormat - default backup name format
	BackupTimeFormat = "2006-01-02T15-04-05"
	hashfile         = "parts.hash"
	MetaFileName     = "metadata.json"
)

var (
	// ErrUnknownClickhouseDataPath -
	ErrUnknownClickhouseDataPath = errors.New("clickhouse data path is unknown, you can set data_path in config file")
)

func addTable(tables []clickhouse.Table, table clickhouse.Table) []clickhouse.Table {
	for _, t := range tables {
		if (t.Database == table.Database) && (t.Name == table.Name) {
			return tables
		}
	}
	return append(tables, table)
}

func addBackupTable(tables clickhouse.BackupTables, table metadata.TableMetadata) clickhouse.BackupTables {
	for _, t := range tables {
		if (t.Database == table.Database) && (t.Table == table.Table) {
			return tables
		}
	}
	return append(tables, table)
}

func filterTablesByPattern(tables []clickhouse.Table, tablePattern string) []clickhouse.Table {
	if tablePattern == "" {
		return tables
	}
	tablePatterns := strings.Split(tablePattern, ",")
	var result []clickhouse.Table
	for _, t := range tables {
		for _, pattern := range tablePatterns {
			if matched, _ := filepath.Match(pattern, fmt.Sprintf("%s.%s", t.Database, t.Name)); matched {
				result = addTable(result, t)
			}
		}
	}
	return result
}

// NewBackupName - return default backup name
func NewBackupName() string {
	return time.Now().UTC().Format(BackupTimeFormat)
}

// CreateBackup - create new backup of all tables matched by tablePattern
// If backupName is empty string will use default backup name
func CreateBackup(cfg config.Config, backupName, tablePattern string) error {
	if backupName == "" {
		backupName = NewBackupName()
	}
	ctx := log.WithFields(log.Fields{
		"backup":    backupName,
		"operation": "create",
	})
	log.SetLevel(log.DebugLevel)
	ch := &clickhouse.ClickHouse{
		Config: &cfg.ClickHouse,
	}
	if err := ch.Connect(); err != nil {
		return fmt.Errorf("can't connect to clickhouse: %v", err)
	}
	defer ch.Close()

	allTables, err := ch.GetTables()
	if err != nil {
		return fmt.Errorf("cat't get tables from clickhouse: %v", err)
	}
	tables := filterTablesByPattern(allTables, tablePattern)
	i := 0
	for _, table := range tables {
		if table.Skip {
			continue
		}
		i++
	}
	if i == 0 {
		return fmt.Errorf("no tables for backup")
	}

	disks, err := ch.GetDisks()
	if err != nil {
		return err
	}
	for _, disk := range disks {
		if err := ch.Mkdir(path.Join(disk.Path, "backup")); err != nil {
			return err
		}
		backupPath := path.Join(disk.Path, "backup", backupName)
		if _, err := os.Stat(backupPath); err == nil || !os.IsNotExist(err) {
			return fmt.Errorf("'%s' already exists", backupPath)
		}
	}
	diskMap := map[string]string{}
	for _, disk := range disks {
		diskMap[disk.Name] = disk.Path
	}
	var backupSize int64
	t := []metadata.TableTitle{}
	for _, table := range tables {
		if table.Skip {
			continue
		}
		ctx.Infof("%s.%s", table.Database, table.Name)
		if err := AddTableToBackup(ch, backupName, &table); err != nil {
			ctx.Errorf("error=\"%v\"", err)
			// TODO: clean bad backup
			continue
		}
		t = append(t, metadata.TableTitle{
			Database: table.Database,
			Table:    table.Name,
		})
		backupSize += table.TotalBytes.Int64
	}
	backupMetafile := metadata.BackupMetadata{
		BackupName:              backupName,
		Disks:                   diskMap,
		ClickhouseBackupVersion: "unknown",
		CreationDate:            time.Now().UTC(),
		// Tags: ,
		// ClickHouseVersion: ch.GetVersion(),
		Size: backupSize,
		// CompressedSize: ,
		Tables: t,
	}
	content, err := json.MarshalIndent(&backupMetafile, "", "\t")
	if err != nil {
		return fmt.Errorf("can't marshal backup metafile json: %v", err)
	}
	defaultPath, err := ch.GetDefaultPath()
	if err != nil {
		return err
	}
	backupMetaFile := path.Join(defaultPath, "backup", backupName, "metadata.json")
	if err := ioutil.WriteFile(backupMetaFile, content, 0640); err != nil {
		return err
	}
	ch.Chown(backupMetaFile)

	if err := RemoveOldBackupsLocal(cfg); err != nil {
		return err
	}
	ctx.Info("done")
	return nil
}

func AddTableToBackup(ch *clickhouse.ClickHouse, backupName string, table *clickhouse.Table) error {
	ctx := log.WithFields(log.Fields{
		"backup":    backupName,
		"operation": "create",
		"table":     fmt.Sprintf("%s.%s", table.Database, table.Name),
	})
	if backupName == "" {
		return fmt.Errorf("backupName is not defined")
	}
	defaultPath, err := ch.GetDefaultPath()
	if err != nil {
		return fmt.Errorf("can't get default data path: %v", err)
	}
	diskList, err := ch.GetDisks()
	if err != nil {
		return fmt.Errorf("can't get clickhouse disk list: %v", err)
	}
	relevantBackupPath := path.Join("backup", backupName)

	//  TODO: дичь какая-то
	diskPathList := []string{defaultPath}
	for _, dataPath := range table.DataPaths {
		for _, disk := range diskList {
			if disk.Path == defaultPath {
				continue
			}
			if strings.HasPrefix(dataPath, disk.Path) {
				diskPathList = append(diskPathList, disk.Path)
				break
			}
		}
	}
	for _, diskPath := range diskPathList {
		backupPath := path.Join(diskPath, relevantBackupPath)
		if err := ch.Mkdir(backupPath); err != nil {
			return err
		}
	}

	ctx.Debug("create metadata")
	backupPath := path.Join(defaultPath, "backup", backupName)
	if err := createMetadata(ch, backupPath, table); err != nil {
		return err
	}
	// backup data
	if !strings.HasSuffix(table.Engine, "MergeTree") {
		return nil
	}
	ctx.Debug("freeze")
	if err := ch.FreezeTable(table); err != nil {
		for _, diskPath := range diskPathList {
			// Remove failed backup
			os.RemoveAll(path.Join(diskPath, relevantBackupPath))
		}
		return err
	}

	ctx.Debug("move shadow")
	for _, diskPath := range diskPathList {
		backupPath := path.Join(diskPath, "backup", backupName)
		shadowPath := path.Join(diskPath, "shadow")
		backupShadowPath := path.Join(backupPath, "shadow")
		if err := moveShadow(shadowPath, backupShadowPath); err != nil {
			return err
		}
		// fix 19.15.3.6
		badTablePath := path.Join(backupShadowPath, table.Database, table.Name)
		if _, err := os.Stat(badTablePath); os.IsNotExist(err) {
			continue
		}
		encodedDBPath := path.Join(backupShadowPath, clickhouse.TablePathEncode(table.Database))
		if err := ch.Mkdir(encodedDBPath); err != nil {
			return err
		}
		encodedTablePath := path.Join(encodedDBPath, clickhouse.TablePathEncode(table.Name))
		if err := os.Rename(badTablePath, encodedTablePath); err != nil {
			return err
		}
		badDBPath := path.Join(path.Join(backupShadowPath, table.Database))
		if err := os.Remove(badDBPath); err != nil {
			return err
		}
	}
	if err := ch.CleanShadow(); err != nil {
		return err
	}
	return nil
}

func createMetadata(ch *clickhouse.ClickHouse, backupPath string, table *clickhouse.Table) error {
	parts, err := ch.GetPartitions(*table)
	if err != nil {
		return err
	}
	metadata := &metadata.TableMetadata{
		Table:      table.Name,
		Database:   table.Database,
		Query:      table.CreateTableQuery,
		UUID:       table.UUID,
		TotalBytes: table.TotalBytes.Int64,
		Parts:      parts,
	}
	metadataPath := path.Join(backupPath, "metadata")
	if err := ch.Mkdir(metadataPath); err != nil {
		return err
	}
	metadataDatabasePath := path.Join(metadataPath, clickhouse.TablePathEncode(table.Database))
	if err := ch.Mkdir(metadataDatabasePath); err != nil {
		return err
	}
	metadataFile := path.Join(metadataDatabasePath, fmt.Sprintf("%s.json", clickhouse.TablePathEncode(table.Name)))
	metadataBody, err := json.MarshalIndent(metadata, "", " ")
	if err != nil {
		return fmt.Errorf("can't marshal %s: %v", MetaFileName, err)
	}
	if err := ioutil.WriteFile(metadataFile, metadataBody, 0644); err != nil {
		return fmt.Errorf("can't create %s: %v", MetaFileName, err)
	}
	if err := ch.Chown(metadataFile); err != nil {
		return err
	}
	return nil
}