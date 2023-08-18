package main

import (
	"errors"
	"fmt"
	"github.com/filecoin-project/boost/storagemarket/types/dealcheckpoints"
	"golang.org/x/xerrors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	bcli "github.com/filecoin-project/boost/cli"
	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/mitchellh/go-homedir"
	"github.com/urfave/cli/v2"
)

func downloadFile(localPath string, remotePath string) error {
	file, err := os.Create(localPath)
	if err != nil {
		return err
	}

	defer func() {
		_ = file.Close()
	}()

	rsp, err := http.Get(remotePath)
	defer func() {
		_ = rsp.Body.Close()
	}()
	if err != nil {
		return err
	}

	if rsp.StatusCode != 200 {
		return xerrors.Errorf("down file error code: %d", rsp.StatusCode)
	}
	_, err = io.Copy(file, rsp.Body)
	return err
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	//isnotexist来判断，是不是不存在的错误
	if os.IsNotExist(err) { //如果返回的错误类型使用os.isNotExist()判断为true，说明文件或者文件夹不存在
		return false, nil
	}
	return false, err //如果有错误了，但是不是不存在的错误，所以把这个错误原封不动的返回
}

var importDataCmd = &cli.Command{
	Name:      "import-data",
	Usage:     "Import data for offline deal made with Boost",
	ArgsUsage: "<proposal CID> <file> or <deal UUID> <file>",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "delete-after-import",
			Usage: "whether to delete the data for the offline deal after the deal has been added to a sector",
			Value: true,
		},
		&cli.BoolFlag{
			Name:  "remote",
			Usage: "is it a remote file",
			Value: true,
		},
		&cli.StringFlag{
			Name:  "remote-path",
			Usage: "remote file download path",
		},
		&cli.StringFlag{
			Name:  "local-path",
			Usage: "local file path",
		},
	},
	Action: func(cctx *cli.Context) error {
		if cctx.Args().Len() < 2 {
			return fmt.Errorf("must specify proposal CID / deal UUID and file path")
		}

		id := cctx.Args().Get(0)
		fileName := cctx.Args().Get(1)

		localPath := cctx.String("local-path")
		if localPath == "" {
			return errors.New("local-path not empty")
		}
		if strings.HasSuffix(localPath, "/") {
			localPath = fmt.Sprintf("%s%s", localPath, fileName)
		} else {
			localPath = fmt.Sprintf("%s/%s", localPath, fileName)
		}

		// Parse the first parameter as a deal UUID or a proposal CID
		var proposalCid *cid.Cid
		dealUuid, err := uuid.Parse(id)
		if err != nil {
			propCid, err := cid.Decode(id)
			if err != nil {
				return fmt.Errorf("could not parse '%s' as deal uuid or proposal cid", id)
			}
			proposalCid = &propCid
		}

		napi, closer, err := bcli.GetBoostAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()

		pds, err := napi.BoostDeal(cctx.Context, dealUuid)
		if err != nil {
			return err
		}

		if pds.Checkpoint != dealcheckpoints.Accepted {
			return xerrors.Errorf("the order %s has been imported", dealUuid.String())
		}

		// 远程下载文件
		remote := cctx.Bool("remote")
		localFileExists, _ := pathExists(localPath)
		if !localFileExists {
			if remote {
				remotePath := cctx.String("remote-path")
				if remotePath == "" {
					return errors.New("remote-path not empty")
				}
				if strings.HasSuffix(remotePath, "/") {
					remotePath = fmt.Sprintf("%s%s", remotePath, fileName)
				} else {
					remotePath = fmt.Sprintf("%s/%s", remotePath, fileName)
				}

				if err := downloadFile(localPath, remotePath); err != nil {
					_ = os.RemoveAll(localPath)
					return err
				}
			} else {
				return errors.New("local file does not exist")
			}
		}

		path, err := homedir.Expand(localPath)
		if err != nil {
			return fmt.Errorf("expanding file path: %w", err)
		}

		filePath, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("failed get absolute path for file: %w", err)
		}

		_, err = os.Stat(filePath)
		if err != nil {
			return fmt.Errorf("opening file %s: %w", filePath, err)
		}

		// If the user has supplied a signed proposal cid
		deleteAfterImport := cctx.Bool("delete-after-import")
		if proposalCid != nil {
			if deleteAfterImport {
				return fmt.Errorf("legacy deal data cannot be automatically deleted after import (only new deals)")
			}

			// Look up the deal in the boost database
			deal, err := napi.BoostDealBySignedProposalCid(cctx.Context, *proposalCid)
			if err != nil {
				// If the error is anything other than a Not Found error,
				// return the error
				if !strings.Contains(err.Error(), "not found") {
					return err
				}

				// The deal is not in the boost database, try the legacy
				// markets datastore (v1.1.0 deal)
				err := napi.MarketImportDealData(cctx.Context, *proposalCid, filePath)
				if err != nil {
					return fmt.Errorf("couldnt import v1.1.0 deal, or find boost deal: %w", err)
				}

				fmt.Printf("Offline deal import for v1.1.0 deal %s scheduled for execution\n", proposalCid.String())
				return nil
			}

			// Get the deal UUID from the deal
			dealUuid = deal.DealUuid
		}

		// Deal proposal by deal uuid (v1.2.0 deal)
		rej, err := napi.BoostOfflineDealWithData(cctx.Context, dealUuid, filePath, deleteAfterImport)
		if err != nil {
			return fmt.Errorf("failed to execute offline deal: %w", err)
		}
		if rej != nil && rej.Reason != "" {
			return fmt.Errorf("offline deal %s rejected: %s", dealUuid, rej.Reason)
		}
		fmt.Printf("Offline deal import for v1.2.0 deal %s scheduled for execution\n", dealUuid)
		return nil
	},
}
