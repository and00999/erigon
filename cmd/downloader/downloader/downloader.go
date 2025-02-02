package downloader

import (
	"context"
	"fmt"
	"runtime"
	"time"

	lg "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/c2h5oh/datasize"
	common2 "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/log/v3"
	"golang.org/x/time/rate"
)

const ASSERT = false

type Client struct {
	Client *torrent.Client
}

func DefaultTorrentConfig() *torrent.ClientConfig {
	torrentConfig := torrent.NewDefaultClientConfig()

	// enable dht
	torrentConfig.NoDHT = true
	//torrentConfig.DisableTrackers = true
	//torrentConfig.DisableWebtorrent = true
	//torrentConfig.DisableWebseeds = true

	// Increase default timeouts, because we often run on commodity networks
	torrentConfig.MinDialTimeout = 6 * time.Second      // default: 3sec
	torrentConfig.NominalDialTimeout = 20 * time.Second // default: 20sec
	torrentConfig.HandshakesTimeout = 8 * time.Second   // default: 4sec

	// We would-like to reduce amount of goroutines in Erigon, so reducing next params
	torrentConfig.EstablishedConnsPerTorrent = 5 // default: 50
	torrentConfig.TorrentPeersHighWater = 10     // default: 500
	torrentConfig.TorrentPeersLowWater = 5       // default: 50
	torrentConfig.HalfOpenConnsPerTorrent = 5    // default: 25
	torrentConfig.TotalHalfOpenConns = 10        // default: 100
	return torrentConfig
}

func TorrentConfig(snapshotsDir string, seeding bool, verbosity lg.Level, downloadRate, uploadRate datasize.ByteSize, torrentPort int) (*torrent.ClientConfig, error) {
	torrentConfig := DefaultTorrentConfig()
	torrentConfig.ListenPort = torrentPort
	torrentConfig.Seed = seeding
	torrentConfig.DataDir = snapshotsDir
	torrentConfig.UpnpID = torrentConfig.UpnpID + "leecher"

	// rates are divided by 2 - I don't know why it works, maybe bug inside torrent lib accounting
	torrentConfig.UploadRateLimiter = rate.NewLimiter(rate.Limit(uploadRate.Bytes()/2), 2*DefaultPieceSize)     // default: unlimited
	torrentConfig.DownloadRateLimiter = rate.NewLimiter(rate.Limit(downloadRate.Bytes()/2), 2*DefaultPieceSize) // default: unlimited

	// debug
	if lg.Debug == verbosity {
		torrentConfig.Debug = true
	}
	torrentConfig.Logger = NewAdapterLogger().FilterLevel(verbosity)

	torrentConfig.DefaultStorage = storage.NewMMap(snapshotsDir)
	return torrentConfig, nil
}

func New(cfg *torrent.ClientConfig, downloaderDB kv.RwDB) (*Client, error) {
	peerID, err := readPeerID(downloaderDB)
	if err != nil {
		return nil, fmt.Errorf("get peer id: %w", err)
	}
	cfg.PeerID = string(peerID)
	torrentClient, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("fail to start torrent client: %w", err)
	}
	if len(peerID) == 0 {
		if err = savePeerID(downloaderDB, torrentClient.PeerID()); err != nil {
			return nil, fmt.Errorf("save peer id: %w", err)
		}
	}

	return &Client{
		Client: torrentClient,
	}, nil
}

func savePeerID(db kv.RwDB, peerID torrent.PeerID) error {
	return db.Update(context.Background(), func(tx kv.RwTx) error {
		return tx.Put(kv.BittorrentInfo, []byte(kv.BittorrentPeerID), peerID[:])
	})
}

func readPeerID(db kv.RoDB) (peerID []byte, err error) {
	if err = db.View(context.Background(), func(tx kv.Tx) error {
		peerIDFromDB, err := tx.GetOne(kv.BittorrentInfo, []byte(kv.BittorrentPeerID))
		if err != nil {
			return fmt.Errorf("get peer id: %w", err)
		}
		peerID = common2.Copy(peerIDFromDB)
		return nil
	}); err != nil {
		return nil, err
	}
	return peerID, nil
}

func (cli *Client) Close() {
	for _, tr := range cli.Client.Torrents() {
		tr.Drop()
	}
	cli.Client.Close()
}

func (cli *Client) PeerID() []byte {
	peerID := cli.Client.PeerID()
	return peerID[:]
}

func MainLoop(ctx context.Context, torrentClient *torrent.Client) {
	interval := time.Second * 5
	logEvery := time.NewTicker(interval)
	defer logEvery.Stop()
	var m runtime.MemStats
	var stats AggStats

	for {
		select {
		case <-ctx.Done():
			return
		case <-logEvery.C:
			torrents := torrentClient.Torrents()
			allComplete := true
			gotInfo := 0
			for _, t := range torrents {
				select {
				case <-t.GotInfo(): // all good
					gotInfo++
				default:
					t.AllowDataUpload()
					t.AllowDataDownload()
				}
				allComplete = allComplete && t.Complete.Bool()
			}
			if gotInfo < len(torrents) {
				log.Info(fmt.Sprintf("[torrent] Waiting for torrents metadata: %d/%d", gotInfo, len(torrents)))
				continue
			}

			runtime.ReadMemStats(&m)
			stats = CalcStats(stats, interval, torrentClient)
			if allComplete {
				log.Info("[torrent] Seeding",
					"download", common2.ByteCount(uint64(stats.readBytesPerSec))+"/s",
					"upload", common2.ByteCount(uint64(stats.writeBytesPerSec))+"/s",
					"peers", stats.peersCount,
					"torrents", stats.torrentsCount,
					"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys))
				continue
			}

			log.Info("[torrent] Downloading",
				"Progress", fmt.Sprintf("%.2f%%", stats.Progress),
				"download", common2.ByteCount(uint64(stats.readBytesPerSec))+"/s",
				"upload", common2.ByteCount(uint64(stats.writeBytesPerSec))+"/s",
				"peers", stats.peersCount,
				"torrents", stats.torrentsCount,
				"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys))
			if stats.peersCount == 0 {
				ips := torrentClient.BadPeerIPs()
				if len(ips) > 0 {
					log.Info("[torrent] Stats", "banned", ips)
				}

			}
		}
	}
}

func (cli *Client) StopSeeding(hash metainfo.Hash) error {
	t, ok := cli.Client.Torrent(hash)
	if !ok {
		return nil
	}
	ch := t.Closed()
	t.Drop()
	<-ch
	return nil
}

type AggStats struct {
	readBytesPerSec  int64
	writeBytesPerSec int64
	peersCount       int64

	Progress      float32
	torrentsCount int

	bytesRead    int64
	bytesWritten int64
}

func CalcStats(prevStats AggStats, interval time.Duration, client *torrent.Client) (result AggStats) {
	var aggBytesCompleted, aggLen int64
	//var aggCompletedPieces, aggNumPieces, aggPartialPieces int
	peers := map[torrent.PeerID]*torrent.PeerConn{}
	torrents := client.Torrents()
	for _, t := range torrents {
		stats := t.Stats()
		/*
			var completedPieces, partialPieces int
			psrs := t.PieceStateRuns()
			for _, r := range psrs {
				if r.Complete {
					completedPieces += r.Length
				}
				if r.Partial {
					partialPieces += r.Length
				}
			}
			aggCompletedPieces += completedPieces
			aggPartialPieces += partialPieces
			aggNumPieces = t.NumPieces()
		*/
		result.bytesRead += stats.BytesRead.Int64() + stats.BytesReadData.Int64()
		result.bytesWritten += stats.BytesWritten.Int64() + stats.BytesWrittenData.Int64()
		aggBytesCompleted += t.BytesCompleted()
		aggLen += t.Length()
		for _, peer := range t.PeerConns() {
			peers[peer.PeerID] = peer
		}
	}

	result.readBytesPerSec += (result.bytesRead - prevStats.bytesRead) / int64(interval.Seconds())
	result.writeBytesPerSec += (result.bytesWritten - prevStats.bytesWritten) / int64(interval.Seconds())

	result.Progress = float32(float64(100) * (float64(aggBytesCompleted) / float64(aggLen)))

	result.peersCount = int64(len(peers))
	result.torrentsCount = len(torrents)
	return result
}

// AddTorrentFiles - adding .torrent files to torrentClient (and checking their hashes), if .torrent file
// added first time - pieces verification process will start (disk IO heavy) - Progress
// kept in `piece completion storage` (surviving reboot). Once it done - no disk IO needed again.
// Don't need call torrent.VerifyData manually
func AddTorrentFiles(snapshotsDir string, torrentClient *torrent.Client) error {
	files, err := AllTorrentPaths(snapshotsDir)
	if err != nil {
		return err
	}
	for _, torrentFilePath := range files {
		mi, err := metainfo.LoadFromFile(torrentFilePath)
		if err != nil {
			return err
		}
		mi.AnnounceList = Trackers

		if _, err = torrentClient.AddTorrent(mi); err != nil {
			return err
		}
	}

	return nil
}

// ResolveAbsentTorrents - add hard-coded hashes (if client doesn't have) as magnet links and download everything
func ResolveAbsentTorrents(ctx context.Context, torrentClient *torrent.Client, preverifiedHashes []metainfo.Hash, snapshotDir string) error {
	mi := &metainfo.MetaInfo{AnnounceList: Trackers}
	for _, infoHash := range preverifiedHashes {
		if _, ok := torrentClient.Torrent(infoHash); ok {
			continue
		}
		magnet := mi.Magnet(&infoHash, nil)
		t, err := torrentClient.AddMagnet(magnet.String())
		if err != nil {
			return err
		}
		t.AllowDataDownload()
		t.AllowDataUpload()
	}

	for _, t := range torrentClient.Torrents() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.GotInfo():
			mi := t.Metainfo()
			if err := CreateTorrentFileIfNotExists(snapshotDir, t.Info(), &mi); err != nil {
				return err
			}
		}
	}

	return nil
}

//nolint
func waitForChecksumVerify(ctx context.Context, torrentClient *torrent.Client) {
	//TODO: tr.VerifyData() - find when to call it
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		interval := time.Second * 5
		logEvery := time.NewTicker(interval)
		defer logEvery.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-logEvery.C:
				var aggBytesCompleted, aggLen int64
				for _, t := range torrentClient.Torrents() {
					aggBytesCompleted += t.BytesCompleted()
					aggLen += t.Length()
				}

				line := fmt.Sprintf(
					"[torrent] verifying snapshots: %s/%s",
					common2.ByteCount(uint64(aggBytesCompleted)),
					common2.ByteCount(uint64(aggLen)),
				)
				log.Info(line)
			}
		}
	}()
	torrentClient.WaitAll() // wait for checksum verify
}

func VerifyDtaFiles(ctx context.Context, snapshotDir string) error {
	logEvery := time.NewTicker(5 * time.Second)
	defer logEvery.Stop()
	files, err := AllTorrentPaths(snapshotDir)
	if err != nil {
		return err
	}
	totalPieces := 0
	for _, f := range files {
		metaInfo, err := metainfo.LoadFromFile(f)
		if err != nil {
			return err
		}
		info, err := metaInfo.UnmarshalInfo()
		if err != nil {
			return err
		}
		totalPieces += info.NumPieces()
	}

	j := 0
	for _, f := range files {
		metaInfo, err := metainfo.LoadFromFile(f)
		if err != nil {
			return err
		}
		info, err := metaInfo.UnmarshalInfo()
		if err != nil {
			return err
		}
		err = verifyTorrent(&info, snapshotDir, func(i int, good bool) error {
			j++
			if !good {
				log.Error("[torrent] Verify hash mismatch", "at piece", i, "file", f)
				return fmt.Errorf("invalid file")
			}
			select {
			case <-logEvery.C:
				log.Info("[torrent] Verify", "Progress", fmt.Sprintf("%.2f%%", 100*float64(j)/float64(totalPieces)))
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	log.Info("[torrent] Verify succeed")
	return nil
}
