package engine //main file for all the calculations and data gathering needed for creating the running torrent arrays

import (
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/asdine/storm"
	Settings "github.com/deranjer/goTorrent/settings"
	Storage "github.com/deranjer/goTorrent/storage"
	"github.com/gorilla/websocket"
	"github.com/mmcdole/gofeed"
	"github.com/sirupsen/logrus"
)

//Logger is the injected variable for global logger
var Logger *logrus.Logger

//Config is the injected variable for the torrent config
var Config Settings.FullClientSettings

//Conn is the injected variable for the websocket connection
var Conn *websocket.Conn

//CreateServerPushMessage Pushes a message from the server to the client
func CreateServerPushMessage(message ServerPushMessage, conn *websocket.Conn) {
	conn.WriteJSON(message)
}

func QueueJSONMessage(conn *websocket.Conn) {

}

//RefreshSingleRSSFeed refreshing a single RSS feed to send to the client (so no updating database) mainly by updating the torrent list to display any changes
func RefreshSingleRSSFeed(db *storm.DB, RSSFeed Storage.SingleRSSFeed) Storage.SingleRSSFeed { //Todo.. duplicate as cron job... any way to merge these to reduce duplication?
	singleRSSFeed := Storage.SingleRSSFeed{URL: RSSFeed.URL, Name: RSSFeed.Name}
	singleRSSTorrent := Storage.SingleRSSTorrent{}
	fp := gofeed.NewParser()
	feed, err := fp.ParseURL(RSSFeed.URL)
	if err != nil {
		Logger.WithFields(logrus.Fields{"RSSFeedURL": RSSFeed.URL, "error": err}).Error("Unable to parse URL")
		CreateServerPushMessage(ServerPushMessage{MessageType: "serverPushMessage", MessageLevel: "error", Payload: "Unable to add Storage Path"}, Conn)
	}
	for _, RSSTorrent := range feed.Items {
		singleRSSTorrent.Link = RSSTorrent.Link
		singleRSSTorrent.Title = RSSTorrent.Title
		singleRSSTorrent.PubDate = RSSTorrent.Published
		singleRSSFeed.Torrents = append(singleRSSFeed.Torrents, singleRSSTorrent)

	}
	return singleRSSFeed

}

//ForceRSSRefresh forces a refresh (in addition to the cron schedule) to add the new RSS feed
func ForceRSSRefresh(db *storm.DB, RSSFeedStore Storage.RSSFeedStore) { //Todo.. duplicate as cron job... any way to merge these to reduce duplication?
	singleRSSTorrent := Storage.SingleRSSTorrent{}
	newFeedStore := Storage.RSSFeedStore{ID: RSSFeedStore.ID} //creating a new feed store just using old one to parse for new torrents
	fp := gofeed.NewParser()
	Logger.WithFields(logrus.Fields{"RSSFeedStoreLength": len(RSSFeedStore.RSSFeeds)}).Debug("Length of RSS feeds (should be ONE)")
	for _, singleFeed := range RSSFeedStore.RSSFeeds {
		feed, err := fp.ParseURL(singleFeed.URL)
		if err != nil {
			Logger.WithFields(logrus.Fields{"RSSFeedURL": singleFeed.URL, "error": err}).Error("Unable to parse RSS URL")
			CreateServerPushMessage(ServerPushMessage{MessageType: "serverPushMessage", MessageLevel: "error", Payload: "Unable to parse RSS URL"}, Conn)
		}
		for _, RSSTorrent := range feed.Items {
			singleRSSTorrent.Link = RSSTorrent.Link
			singleRSSTorrent.Title = RSSTorrent.Title
			singleRSSTorrent.PubDate = RSSTorrent.Published
			singleFeed.Torrents = append(singleFeed.Torrents, singleRSSTorrent)

		}
		newFeedStore.RSSFeeds = append(newFeedStore.RSSFeeds, singleFeed)
	}
	Storage.UpdateRSSFeeds(db, newFeedStore) //Calling this to fully update storage will all rss feeds
}

//timeOutInfo forcing a timeout of the torrent if it doesn't load from program restart
func timeOutInfo(clientTorrent *torrent.Torrent, seconds time.Duration) (deleted bool) {
	Logger.WithFields(logrus.Fields{"Seconds to wait for info...": seconds}).Info("Attempting to download info for torrent")
	timeout := make(chan bool, 1) //creating a timeout channel for our gotinfo
	go func() {
		time.Sleep(seconds * time.Second)
		timeout <- true
	}()
	select {
	case <-clientTorrent.GotInfo(): //attempting to retrieve info for torrent
		Logger.WithFields(logrus.Fields{"clientTorrentName": clientTorrent.Name()}).Debug("Received torrent info for torrent")
		return false
	case <-timeout: // getting info for torrent has timed out so purging the torrent
		Logger.WithFields(logrus.Fields{"clientTorrentName": clientTorrent.Name()}).Error("Forced to drop torrent from timeout waiting for info")
		CreateServerPushMessage(ServerPushMessage{MessageType: "serverPushMessage", MessageLevel: "error", Payload: "Timout waiting for torrent info... dropping"}, Conn)
		clientTorrent.Drop()
		return true
	}

}

func readTorrentFileFromDB(element *Storage.TorrentLocal, tclient *torrent.Client, db *storm.DB) (singleTorrent *torrent.Torrent, err error) {
	tempFile, err := ioutil.TempFile("", "TorrentFileTemp")
	if err != nil {
		Logger.WithFields(logrus.Fields{"tempfile": tempFile, "err": err}).Error("Unable to create tempfile")
		return nil, err
	}
	//defer tempFile.Close() //Todo.. if we remove this do we need to close it?
	defer os.Remove(tempFile.Name())
	if _, err := tempFile.Write(element.TorrentFile); err != nil { //writing out out the entire file back into the temp dir from boltdb
		Logger.WithFields(logrus.Fields{"tempfile": tempFile, "err": err}).Error("Unable to write to tempfile")
		return nil, err
	}
	if err := tempFile.Close(); err != nil { //close the tempfile so that we can add it back into the torrent client
		Logger.WithFields(logrus.Fields{"tempfile": tempFile, "err": err}).Error("Unable to close tempfile")
	}
	_, err = os.Stat(element.TorrentFileName) //if we CAN find the torrent, add it
	if err != nil {
		Logger.WithFields(logrus.Fields{"tempfile": tempFile, "err": err}).Error("Unable to find file")
		Storage.DelTorrentLocalStorage(db, element.Hash) //purge the torrent
		return nil, err
	}
	singleTorrent, err = tclient.AddTorrentFromFile(element.TorrentFileName)
	if err != nil {
		Logger.WithFields(logrus.Fields{"tempfile": element.TorrentFileName, "err": err}).Error("Unable to add Torrent from file!")
		CreateServerPushMessage(ServerPushMessage{MessageType: "serverPushMessage", MessageLevel: "error", Payload: "Unable to add Torrent from file!"}, Conn)
		Storage.DelTorrentLocalStorage(db, element.Hash) //purge the torrent
		return nil, err
	}
	return singleTorrent, nil
}

//AddTorrent creates the storage.db entry and starts A NEW TORRENT and adds to the running torrent array
func AddTorrent(clientTorrent *torrent.Torrent, torrentLocalStorage Storage.TorrentLocal, db *storm.DB, torrentType, torrentFilePathAbs, torrentStoragePath, labelValue string, config Settings.FullClientSettings) {
	timedOut := timeOutInfo(clientTorrent, 45) //seeing if adding the torrent times out (giving 45 seconds)
	if timedOut {                              //if we fail to add the torrent return
		return
	}
	var TempHash metainfo.Hash
	TempHash = clientTorrent.InfoHash()
	fmt.Println("GOT INFOHASH", TempHash.String())
	allStoredTorrents := Storage.FetchAllStoredTorrents(db)
	for _, runningTorrentHashes := range allStoredTorrents {
		if runningTorrentHashes.Hash == TempHash.String() {
			Logger.WithFields(logrus.Fields{"Hash": TempHash.String()}).Info("Torrent has duplicate hash to already running torrent... will not add to storage")
			return
		}
	}
	torrentLocalStorage.Hash = TempHash.String() // we will store the infohash to add it back later on client restart (if needed)
	torrentLocalStorage.InfoBytes = clientTorrent.Metainfo().InfoBytes
	torrentLocalStorage.Label = labelValue
	torrentLocalStorage.DateAdded = time.Now().Format("Jan _2 2006")
	torrentLocalStorage.StoragePath = torrentStoragePath
	torrentLocalStorage.TempStoragePath = config.TorrentConfig.DataDir
	torrentLocalStorage.TorrentName = clientTorrent.Name()
	torrentLocalStorage.TorrentUploadLimit = true            //by default all of the torrents will stop uploading after the global rate is set.
	torrentLocalStorage.TorrentMoved = false                 //by default the torrent has no been moved.
	torrentLocalStorage.TorrentStatus = "Running"            //by default start all the torrents as downloading.
	torrentLocalStorage.TorrentType = torrentType            //either "file" or "magnet" maybe more in the future
	torrentLocalStorage.TorrentSize = clientTorrent.Length() //Length will change as we cancel files so store it in DB
	if torrentType == "file" {                               //if it is a file read the entire file into the database for us to spit out later
		torrentfile, err := ioutil.ReadFile(torrentFilePathAbs)
		torrentLocalStorage.TorrentFileName = torrentFilePathAbs
		if err != nil {
			Logger.WithFields(logrus.Fields{"torrentFile": torrentfile, "error": err}).Error("Unable to read the torrent file")
		}
		torrentLocalStorage.TorrentFile = torrentfile //storing the entire file in to database
	}
	Logger.WithFields(logrus.Fields{"Storage Path": torrentStoragePath, "Torrent Name": clientTorrent.Name()}).Info("Adding Torrent with following storage path, to active Queue")
	torrentFiles := clientTorrent.Files() //storing all of the files in the database along with the priority
	var TorrentFilePriorityArray = []Storage.TorrentFilePriority{}
	for _, singleFile := range torrentFiles { //creating the database setup for the file array
		var torrentFilePriority = Storage.TorrentFilePriority{}
		torrentFilePriority.TorrentFilePath = singleFile.DisplayPath()
		torrentFilePriority.TorrentFilePriority = "Normal"
		torrentFilePriority.TorrentFileSize = singleFile.Length()
		TorrentFilePriorityArray = append(TorrentFilePriorityArray, torrentFilePriority)

	}
	torrentLocalStorage.TorrentFilePriority = TorrentFilePriorityArray
	//torrentQueues := Storage.FetchQueues(db)
	AddTorrentToActive(&torrentLocalStorage, clientTorrent, db)
	Storage.AddTorrentLocalStorage(db, torrentLocalStorage) //writing all of the data to the database
}

//CreateInitialTorrentArray adds all the torrents on program start from the database
func CreateInitialTorrentArray(tclient *torrent.Client, TorrentLocalArray []*Storage.TorrentLocal, db *storm.DB, config Settings.FullClientSettings) {
	for _, singleTorrentFromStorage := range TorrentLocalArray {
		var singleTorrent *torrent.Torrent
		var err error
		if singleTorrentFromStorage.TorrentType == "file" { //if it is a file pull it from the uploaded torrent folder
			singleTorrent, err = readTorrentFileFromDB(singleTorrentFromStorage, tclient, db)
			if err != nil {
				continue
			}
		} else {
			singleTorrentFromStorageMagnet := "magnet:?xt=urn:btih:" + singleTorrentFromStorage.Hash //For magnet links just need to prepend the magnet part to the hash to readd
			singleTorrent, err = tclient.AddMagnet(singleTorrentFromStorageMagnet)
			if err != nil {
				continue
			}
		}
		if len(singleTorrentFromStorage.InfoBytes) == 0 { //TODO.. kind of a fringe scenario.. not sure if needed since the db should always have the infobytes
			timeOut := timeOutInfo(singleTorrent, 45)
			if timeOut == true { // if we did timeout then drop the torrent from the bolt.db database
				Storage.DelTorrentLocalStorage(db, singleTorrentFromStorage.Hash) //purging torrent from the local database
				continue
			}
			singleTorrentFromStorage.InfoBytes = singleTorrent.Metainfo().InfoBytes
		}

		err = singleTorrent.SetInfoBytes(singleTorrentFromStorage.InfoBytes) //setting the infobytes back into the torrent
		if err != nil {
			Logger.WithFields(logrus.Fields{"torrentFile": singleTorrent.Name(), "error": err}).Error("Unable to add infobytes to the torrent!")
		}
		torrentQueues := Storage.FetchQueues(db)
		if singleTorrentFromStorage.TorrentStatus == "Stopped" {
			singleTorrent.SetMaxEstablishedConns(0)
			continue
		}
		if len(torrentQueues.ActiveTorrents) == 0 && len(torrentQueues.QueuedTorrents) == 0 { // If empty, run through all the torrents and assign them
			if len(torrentQueues.ActiveTorrents) < Config.MaxActiveTorrents {
				if singleTorrentFromStorage.TorrentStatus == "Completed" || singleTorrentFromStorage.TorrentStatus == "Seeding" {
					Logger.WithFields(logrus.Fields{"Torrent Name": singleTorrentFromStorage.TorrentName}).Info("Completed Torrents have lower priority, adding to Queued")
					AddTorrentToQueue(singleTorrentFromStorage, singleTorrent, db)
				} else {
					Logger.WithFields(logrus.Fields{"Torrent Name": singleTorrentFromStorage.TorrentName}).Info("Adding Torrent to Active Queue (Initial Torrent Load)")
					AddTorrentToActive(singleTorrentFromStorage, singleTorrent, db)
				}
			} else {
				Logger.WithFields(logrus.Fields{"Torrent Name": singleTorrentFromStorage.TorrentName}).Info("Last resort for torrent, adding to Queued")
				AddTorrentToQueue(singleTorrentFromStorage, singleTorrent, db)
			}
		} else { //If we already have a queue set up then assign torrents to queue
			if singleTorrentFromStorage.TorrentStatus == "Queued" {
				AddTorrentToQueue(singleTorrentFromStorage, singleTorrent, db)
			} else {
				if len(torrentQueues.ActiveTorrents) < Config.MaxActiveTorrents {
					Logger.WithFields(logrus.Fields{"Torrent Name": singleTorrentFromStorage.TorrentName}).Info("Adding Torrent to Active Queue (Initial Torrent Load Second)")
					AddTorrentToActive(singleTorrentFromStorage, singleTorrent, db)
				} else {
					AddTorrentToQueue(singleTorrentFromStorage, singleTorrent, db)
				}
			}
			RemoveDuplicatesFromQueues(db)
		}
		Storage.UpdateStorageTick(db, *singleTorrentFromStorage)
	}
	torrentQueues := Storage.FetchQueues(db)
	if len(torrentQueues.ActiveTorrents) < config.MaxActiveTorrents && len(torrentQueues.QueuedTorrents) > 0 { //after all the torrents are added, see if out active torrent list isn't full, then add from the queue
		Logger.WithFields(logrus.Fields{"Max Active: ": config.MaxActiveTorrents, "Current : ": torrentQueues.ActiveTorrents}).Info("Adding Torrents from queue to active to fill...")
		maxCanSend := config.MaxActiveTorrents - len(torrentQueues.ActiveTorrents)
		if maxCanSend > len(torrentQueues.QueuedTorrents) {
			maxCanSend = len(torrentQueues.QueuedTorrents)
		}
		torrentsToStart := make([]string, maxCanSend)
		copy(torrentsToStart, torrentQueues.QueuedTorrents[len(torrentsToStart)-1:])
		for _, torrentStart := range torrentsToStart {
			for _, singleTorrent := range tclient.Torrents() {
				if singleTorrent.InfoHash().String() == torrentStart {
					singleTorrentFromStorage := Storage.FetchTorrentFromStorage(db, torrentStart)
					AddTorrentToActive(&singleTorrentFromStorage, singleTorrent, db)
				}
			}
		}
	}
	SetFilePriority(tclient, db) //Setting the desired file priority from storage
	Logger.WithFields(logrus.Fields{"Max Active: ": config.MaxActiveTorrents, "Current : ": torrentQueues.ActiveTorrents}).Debug("Queue after all initial torrents have been added")
}

//CreateRunningTorrentArray creates the entire torrent list to pass to client
func CreateRunningTorrentArray(tclient *torrent.Client, TorrentLocalArray []*Storage.TorrentLocal, PreviousTorrentArray []ClientDB, config Settings.FullClientSettings, db *storm.DB) (RunningTorrentArray []ClientDB) {
	torrentQueues := Storage.FetchQueues(db)
	Logger.WithFields(logrus.Fields{"Max Active: ": config.MaxActiveTorrents, "TorrentQueues": torrentQueues}).Debug("Current TorrentQueues")
	for _, singleTorrentFromStorage := range TorrentLocalArray {
		torrentQueues := Storage.FetchQueues(db)
		var singleTorrent *torrent.Torrent

		for _, liveTorrent := range tclient.Torrents() { //matching the torrent from storage to the live torrent
			if singleTorrentFromStorage.Hash == liveTorrent.InfoHash().String() {
				singleTorrent = liveTorrent
			}
		}
		tickUpdateStruct := Storage.TorrentLocal{} //we are shoving the tick updates into a torrentlocal struct to pass to storage happens at the end of the routine
		fullClientDB := new(ClientDB)
		//Handling deleted torrents here
		if singleTorrentFromStorage.TorrentStatus == "Dropped" {
			Logger.WithFields(logrus.Fields{"selection": singleTorrentFromStorage.TorrentName}).Info("Deleting just the torrent")
			DeleteTorrentFromQueues(singleTorrentFromStorage.Hash, db)
			singleTorrent.Drop()
			Storage.DelTorrentLocalStorage(db, singleTorrentFromStorage.Hash)
		}
		if singleTorrentFromStorage.TorrentStatus == "DroppedData" {
			Logger.WithFields(logrus.Fields{"selection": singleTorrentFromStorage.TorrentName}).Info("Deleting torrent and data")
			singleTorrent.Drop()
			DeleteTorrentFromQueues(singleTorrentFromStorage.Hash, db)
			Storage.DelTorrentLocalStorageAndFiles(db, singleTorrentFromStorage.Hash, Config.TorrentConfig.DataDir)
		}
		if singleTorrentFromStorage.TorrentType == "file" { //if it is a file pull it from the uploaded torrent folder
			fullClientDB.SourceType = "Torrent File"
		} else {
			fullClientDB.SourceType = "Magnet Link"
		}
		var TempHash metainfo.Hash
		TempHash = singleTorrent.InfoHash()

		calculatedTotalSize := CalculateDownloadSize(singleTorrentFromStorage, singleTorrent)
		calculatedCompletedSize := CalculateCompletedSize(singleTorrentFromStorage, singleTorrent)
		fullStruct := singleTorrent.Stats()
		activePeersString := strconv.Itoa(fullStruct.ActivePeers) //converting to strings
		totalPeersString := fmt.Sprintf("%v", fullStruct.TotalPeers)
		fullClientDB.StoragePath = singleTorrentFromStorage.StoragePath

		downloadedSizeHumanized := HumanizeBytes(float32(calculatedCompletedSize)) //convert size to GB if needed
		totalSizeHumanized := HumanizeBytes(float32(calculatedTotalSize))

		fullClientDB.DownloadedSize = downloadedSizeHumanized
		fullClientDB.Size = totalSizeHumanized
		PercentDone := fmt.Sprintf("%.2f", float32(calculatedCompletedSize)/float32(calculatedTotalSize))
		fullClientDB.TorrentHash = TempHash
		fullClientDB.PercentDone = PercentDone
		fullClientDB.DataBytesRead = fullStruct.ConnStats.BytesReadData.Int64()       //used for calculations not passed to client calculating up/down speed
		fullClientDB.DataBytesWritten = fullStruct.ConnStats.BytesWrittenData.Int64() //used for calculations not passed to client calculating up/down speed
		fullClientDB.ActivePeers = activePeersString + " / (" + totalPeersString + ")"
		fullClientDB.TorrentHashString = TempHash.String()
		fullClientDB.TorrentName = singleTorrentFromStorage.TorrentName
		fullClientDB.DateAdded = singleTorrentFromStorage.DateAdded
		fullClientDB.TorrentLabel = singleTorrentFromStorage.Label
		fullClientDB.BytesCompleted = calculatedCompletedSize
		fullClientDB.NumberofFiles = len(singleTorrent.Files())

		if len(PreviousTorrentArray) > 0 { //if we actually have  a previous array //ranging over the previous torrent array to calculate the speed for each torrent
			for _, previousElement := range PreviousTorrentArray {
				TempHash := singleTorrent.InfoHash()
				if previousElement.TorrentHashString == TempHash.String() { //matching previous to new
					CalculateTorrentSpeed(singleTorrent, fullClientDB, previousElement, calculatedCompletedSize)
					fullClientDB.TotalUploadedBytes = singleTorrentFromStorage.UploadedBytes + (fullStruct.ConnStats.BytesWrittenData.Int64() - previousElement.DataBytesWritten)
				}
			}
		}
		CalculateTorrentETA(singleTorrentFromStorage.TorrentSize, calculatedCompletedSize, fullClientDB) //needs to be here since we need the speed calculated before we can estimate the eta.

		fullClientDB.TotalUploadedSize = HumanizeBytes(float32(fullClientDB.TotalUploadedBytes))
		fullClientDB.UploadRatio = CalculateUploadRatio(singleTorrent, fullClientDB) //calculate the upload ratio

		CalculateTorrentStatus(singleTorrent, fullClientDB, config, singleTorrentFromStorage, calculatedCompletedSize, calculatedTotalSize, torrentQueues, db) //add torrents to the queue, remove from queue, etc

		tickUpdateStruct.UploadRatio = fullClientDB.UploadRatio
		tickUpdateStruct.TorrentSize = calculatedTotalSize
		tickUpdateStruct.UploadedBytes = fullClientDB.TotalUploadedBytes
		tickUpdateStruct.TorrentStatus = fullClientDB.Status
		tickUpdateStruct.Hash = fullClientDB.TorrentHashString //needed for index
		Storage.UpdateStorageTick(db, tickUpdateStruct)
		RunningTorrentArray = append(RunningTorrentArray, *fullClientDB)

	}
	return RunningTorrentArray
}

//CreateFileListArray creates a file list for a single torrent that is selected and sent to the server
func CreateFileListArray(tclient *torrent.Client, selectedHash string, db *storm.DB, config Settings.FullClientSettings) TorrentFileList {
	runningTorrents := tclient.Torrents() //don't need running torrent array since we aren't adding or deleting from storage
	torrentFileListStorage := Storage.FetchTorrentFromStorage(db, selectedHash)
	TorrentFileListSelected := TorrentFileList{}
	TorrentFileStruct := TorrentFile{}
	for _, singleTorrent := range runningTorrents {
		tempHash := singleTorrent.InfoHash().String()
		if tempHash == selectedHash { // if our selection hash equals our torrent hash
			torrentFilesRaw := singleTorrent.Files()
			Logger.WithFields(logrus.Fields{"torrentFiles": torrentFilesRaw}).Debug("Unable to close tempfile")
			for _, singleFile := range torrentFilesRaw {
				TorrentFileStruct.TorrentHashString = tempHash
				TorrentFileStruct.FileName = singleFile.DisplayPath()
				TorrentFileStruct.FilePath = singleFile.Path()
				PieceState := singleFile.State()
				var downloadedBytes int64
				for _, piece := range PieceState {
					if piece.Complete {
						downloadedBytes = downloadedBytes + piece.Bytes //adding up the bytes in the completed pieces
					}
				}
				TorrentFileStruct.FilePercent = fmt.Sprintf("%.2f", float32(downloadedBytes)/float32(singleFile.Length()))

				for i, specificFile := range torrentFileListStorage.TorrentFilePriority { //searching for that specific file in storage
					if specificFile.TorrentFilePath == singleFile.DisplayPath() {
						TorrentFileStruct.FilePriority = torrentFileListStorage.TorrentFilePriority[i].TorrentFilePriority
					}
				}
				TorrentFileStruct.FileSize = HumanizeBytes(float32(singleFile.Length()))
				TorrentFileListSelected.FileList = append(TorrentFileListSelected.FileList, TorrentFileStruct)
			}
			TorrentFileListSelected.MessageType = "torrentFileList"
			TorrentFileListSelected.TotalFiles = len(singleTorrent.Files())
			Logger.WithFields(logrus.Fields{"selectedFiles": TorrentFileListSelected}).Debug("Selected Torrent Files")
			return TorrentFileListSelected
		}

	}
	return TorrentFileListSelected
}

//CreatePeerListArray create a list of peers for the torrent and displays them
func CreatePeerListArray(tclient *torrent.Client, selectedHash string) PeerFileList {
	runningTorrents := tclient.Torrents()
	TorrentPeerList := PeerFileList{}
	for _, singleTorrent := range runningTorrents {
		tempHash := singleTorrent.InfoHash().String()
		if (strings.Compare(tempHash, selectedHash)) == 0 {
			TorrentPeerList.MessageType = "torrentPeerList"
			TorrentPeerList.PeerList = singleTorrent.KnownSwarm()
			TorrentPeerList.TotalPeers = len(TorrentPeerList.PeerList)
			return TorrentPeerList
		}
	}
	return TorrentPeerList
}

//CreateTorrentDetailJSON creates the json response for a request for more torrent information
func CreateTorrentDetailJSON(tclient *torrent.Client, selectedHash string, torrentStorage *storm.DB) ClientDB {

	localTorrentInfo := Storage.FetchTorrentFromStorage(torrentStorage, selectedHash)

	runningTorrents := tclient.Torrents()

	TorrentDetailStruct := ClientDB{}
	for _, singleTorrent := range runningTorrents { //ranging through the running torrents to find the one we are looking for
		tempHash := singleTorrent.InfoHash().String()
		if tempHash == selectedHash {
			Logger.WithFields(logrus.Fields{"torrentHash": tempHash, "detailedInfo": localTorrentInfo}).Info("Creating detailed torrent list")
			return TorrentDetailStruct
		}
	}
	return TorrentDetailStruct
}
