package photobak

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

// Repository is a type that can store media files. It consists
// of a directory path and a database. It has methods to
// interact with providers (Client implementations) with which
// backups can be downloaded to this repository.
//
// A repository's files are totally managed and should not be
// modified, as each one is indexed in the database.
//
// A repository should not be changed after (or at least
// while) it performs a task.
type Repository struct {
	// the path to the directory of the repo. the leaf folder
	// of the path should be empty if it exists.
	path string

	// the database to operate on; should be opened.
	db *boltDB

	// a map of files that are currently being downloaded/updated.
	// key is the item ID, value is a struct which describes
	// current state of the downloading item.
	downloading   map[string]*downloadingItem
	downloadingMu sync.Mutex

	// a map of item path to channel used for waiting; if two
	// different items have same name and path, this map will
	// be used to ensure different filenames for each one.
	itemNames   map[string]chan struct{}
	itemNamesMu sync.Mutex

	// a set of item checksums to channel used for waiting;
	// if two different goroutines download the same content
	// concurrently (because for some reason they have
	// different IDs, happens on Google Photos), this map will
	// ensure that only one checksum is processed at a time.
	itemChecksums   map[string]chan struct{}
	itemChecksumsMu sync.Mutex

	// NumWorkers is how many download workers to operate
	// in parallel.
	NumWorkers int
}

type downloadingItem struct {
	// a path to a file where the item is currently downloading.
	// zero value means that the file either hasn't been created
	// yet or that the item has successfully finished its downloading.
	path   string
	pathMu sync.Mutex

	// a channel used for waiting for item downloading completion
	// (either successful or not).
	completed chan struct{}
}

// Removes the downloading file.
func (i *downloadingItem) remove() {
	if i.path != "" {
		os.Remove(i.path)
		i.path = ""
	}
}

// OpenRepo opens a repository that is ready to store backups
// in. It is initiated with a path, where a folder will be created
// if it does not already exists, and a database will be created
// inside it. The path is where all saved assets will be stored.
// An opened repository should be closed when finished with it.
func OpenRepo(path string) (*Repository, error) {
	err := os.MkdirAll(path, 0700)
	if err != nil {
		return nil, err
	}

	dbPath := filepath.Join(path, "photobak.db")
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}

	// make sure all accounts have a home in the DB
	for _, account := range getAccounts() {
		err := db.createAccount(account)
		if err != nil {
			return nil, err
		}
	}

	return &Repository{
		path:          path,
		db:            db,
		downloading:   make(map[string]*downloadingItem),
		itemNames:     make(map[string]chan struct{}),
		itemChecksums: make(map[string]chan struct{}),
	}, nil
}

// Close closes a repository cleanly.
func (r *Repository) Close() error {
	return r.db.Close()
}

// Unsafe version of Close() which is expected to be called in the
// middle of backing up process right before of os.Exit() call and
// intended to provide a shutdown with best effort cleanup of created
// temporary files and keeping the database in consistent state.
func (r *Repository) CloseUnsafeOnExit() {
	// We're intentionally lock mutexes here without subsequent unlock
	// to avoid a race in the middle of unlock and os.Exit().

	r.downloadingMu.Lock()

	for _, downloadingItem := range r.downloading {
		downloadingItem.pathMu.Lock()

		if downloadingItem.path != "" {
			Info.Printf("Removing partially downloaded %s", r.repoRelative(downloadingItem.path))
			os.Remove(downloadingItem.path)
		}
	}

	r.Close()
}

// getCredentials loads credentials for the given account, or if there
// are none, it will ask for new ones and save them, returning the
// byte representation of the credentials.
func (r *Repository) getCredentials(pa providerAccount) ([]byte, error) {
	// see if credentials are in database already
	creds, err := r.db.loadCredentials(pa)
	if err != nil {
		return nil, fmt.Errorf("loading credentials for %s: %v", pa.username, err)
	}
	if creds == nil {
		fmt.Printf("Credentials needed for %s (%s).\n", pa.username, pa.provider.Title)
		// we need to get credentials to access cloud provider
		creds, err = pa.provider.Credentials(pa.username)
		if err != nil {
			return nil, fmt.Errorf("getting credentials for %s: %v", pa.username, err)
		}
		err = r.db.saveCredentials(pa, creds)
		if err != nil {
			return nil, fmt.Errorf("saving credentials for %s: %v", pa.username, err)
		}
	}
	return creds, nil
}

// AuthorizeAllAccounts will obtain authorization for all
// configured accounts and then store them in the database,
// but will not perform any other tasks.
func (r *Repository) AuthorizeAllAccounts() error {
	_, err := r.authorizedAccounts()
	return err
}

// Store downloads all media from all registered accounts
// and stores it in the repository path. It is idempotent in
// that it can be run multiple times (assuming the same
// accounts are configured) and only the items that need to
// be downloaded will be downloaded to keep things current
// and up-to-date.
//
// If saveEverything is true, the repository will also save
// everything the API provides about each item to the index.
// This will substantially increase the size of the database
// file, but if that extra data (like, say, links to thumbnail
// images or the number of comments on album) is important to
// you, set it to true.
//
// If checkIntegrity is true, consistency of the items that
// are already stored in the database will be checked.
//
// Store operates per-collection (per-album), that is, it
// iterates each collection and downloads all the items for
// each collection, and organizes them by collection name
// on disk.
//
// Store does not download multiple copies of the same
// photo, assuming the provider correctly IDs each item.
// If an item appears in more than one collection, the
// filepath to the item will be written to a text file
// in the other collection.
//
// Store is NOT destructive or re-organizive (is that
// a word?). Collections that are deleted remotely, or items
// that are removed from collections or deleted entirely,
// will not disappear locally by running this method. It
// will, however, update existing items if they are outdated,
// missing, or corrupted locally.
func (r *Repository) Store(saveEverything bool, checkIntegrity bool) error {
	accounts, err := r.authorizedAccounts()
	if err != nil {
		return err
	}

	// prepare to start a number of workers that will perform downloads
	var workerWg sync.WaitGroup
	ctxChan := make(chan itemContext)
	numWorkers := r.NumWorkers
	if numWorkers < 1 {
		numWorkers = 1
	}

	// spawn worker goroutines
	for i := 0; i < numWorkers; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for itemCtx := range ctxChan {
				err := r.processItem(itemCtx)
				if err != nil {
					log.Println(err)
				}
			}
		}()
	}

	// perform downloads for each account
	var collWg sync.WaitGroup
	numCollWorkers := r.NumWorkers / 2
	if numCollWorkers < 1 {
		numCollWorkers = 1
	}
	throttle := make(chan struct{}, numCollWorkers)
	for _, ac := range accounts {
		listedCollections, err := ac.client.ListCollections()
		if err != nil {
			return err
		}
		for _, listedColl := range listedCollections {
			throttle <- struct{}{}
			go func(listedColl Collection) {
				defer func() { <-throttle }()
				err := r.processCollection(listedColl, ac, ctxChan, saveEverything, checkIntegrity, &collWg)
				if err != nil {
					Info.Printf("[ERROR] processing %s: %v", listedColl.CollectionName(), err)
					return
				}
			}(listedColl)
		}
		for i := 0; i < cap(throttle); i++ {
			throttle <- struct{}{} // make sure all goroutines finish
		}
	}

	// block until the processCollection() goroutines have finished
	// wrapping all items; this is important because the context
	// channel needs to be closed once they're done, but it is not
	// safe to close the context channel before we are sure they
	// finish
	collWg.Wait()

	close(ctxChan)

	// block until all the workers are finished
	workerWg.Wait()

	return nil
}

// authorizedAccounts gets a list of all the configured accounts
// and attaches an authorized client to each one; it will obtain
// credentials if needed.
func (r *Repository) authorizedAccounts() ([]accountClient, error) {
	var accounts []accountClient
	for _, pa := range getAccounts() {
		creds, err := r.getCredentials(pa)
		if err != nil {
			return nil, fmt.Errorf("getting credentials: %v", err)
		}
		client, err := pa.provider.NewClient(creds)
		if err != nil {
			return nil, fmt.Errorf("getting authenticated client: %v", err)
		}
		accounts = append(accounts, accountClient{
			account: pa,
			client:  client,
		})
	}
	return accounts, nil
}

// processCollection will process a collection from a provider.
func (r *Repository) processCollection(listedColl Collection, ac accountClient, ctxChan chan itemContext,
	saveEverything bool, checkIntegrity bool, wg *sync.WaitGroup) error {
	Info.Printf("Processing collection %s: %s", listedColl.CollectionID(), listedColl.CollectionName())

	// see if we have the collection in the db already
	dbc, err := r.db.loadCollection(ac.account.key(), listedColl.CollectionID())
	if err != nil {
		return err
	}

	// carefully craft the collection object... if it is a new collection,
	// we need to choose a folder name that's not in use (in case the name
	// is the same as an existing collection), otherwise use existing path.
	coll := collection{Collection: listedColl}
	if dbc == nil {
		// it's new! great, make sure we don't overwrite (merge) with
		// an existing collection of the same name in this account.
		coll.dirName, err = r.reserveUniqueFilename(ac.account.accountPath(), listedColl.CollectionName(), true)
		if err != nil {
			return err
		}
	} else {
		// we've seen this collection before, so use folder already on disk.
		coll.dirName = dbc.DirName
	}
	coll.dirPath = r.repoRelative(filepath.Join(ac.account.accountPath(), coll.dirName))

	// save collection to database
	if dbc == nil {
		dbc = &dbCollection{
			ID:      coll.CollectionID(),
			Name:    coll.CollectionName(),
			DirName: coll.dirName,
			DirPath: coll.dirPath,
			Items:   make(map[string]struct{}),
		}
	}
	dbc.Saved = time.Now()
	if saveEverything {
		dbc.Meta.API = coll.Collection
	}
	err = r.db.saveCollection(ac.account.key(), dbc.ID, dbc)
	if err != nil {
		if dbc == nil {
			// this was a new collection, couldn't save it to DB,
			// so don't leave a stray folder on disk.
			os.Remove(coll.dirPath)
		}
		return fmt.Errorf("saving collection to database: %v", err)
	}

	// for each item that is listed by the client,
	// wrap it in a context and pass it to the workers
	// to do the processing & downloading.
	itemChan := make(chan Item)

	wg.Add(1)
	go func(wg *sync.WaitGroup) {
		defer wg.Done()
		for receivedItem := range itemChan {
			ctxChan <- itemContext{
				item:           receivedItem,
				coll:           coll,
				ac:             ac,
				saveEverything: saveEverything,
				checkIntegrity: checkIntegrity,
			}
		}
	}(wg)

	// begin processing all the items for this collection
	err = ac.client.ListCollectionItems(coll, itemChan)
	if err != nil {
		return fmt.Errorf("client error listing collection items, giving up: %v", err)
	}

	return nil
}

// processItem will process an item from a provider.
func (r *Repository) processItem(ctx itemContext) error {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC] recovered from processItem: %v", r)
		}
	}()

	itemID := ctx.item.ItemID()
	mapKey := ctx.ac.account.provider.Name + ":" + itemID
	downloadingItem := &downloadingItem{completed: make(chan struct{})}

	for {
		r.downloadingMu.Lock()

		if otherDownloadingItem, ok := r.downloading[mapKey]; ok {
			r.downloadingMu.Unlock()

			// it's already being downloaded.
			// waiting for completion of download process...
			<-otherDownloadingItem.completed
		} else {
			// not being downloaded; claim it for us.
			r.downloading[mapKey] = downloadingItem
			r.downloadingMu.Unlock()
			break
		}
	}
	defer func() {
		r.downloadingMu.Lock()
		delete(r.downloading, mapKey)
		r.downloadingMu.Unlock()
		close(downloadingItem.completed)
	}()

	// check if we already have it
	loadedItem, err := r.db.loadItem(ctx.ac.account.key(), itemID)
	if err != nil {
		return fmt.Errorf("loading item '%s' from database: %v", itemID, err)
	}

	if loadedItem != nil {
		// we already have this item in the DB

		_, dbHas := loadedItem.Collections[ctx.coll.CollectionID()]

		if !dbHas || ctx.checkIntegrity {
			// if we don't have it on disk as a file or in the media list file for
			// this collection already, add path to text file in this collection.
			if folderHas, err := r.localCollectionHasItemOnDisk(ctx.ac.account, ctx.coll, loadedItem); err != nil {
				return fmt.Errorf("checking if local collection has item: %v", err)
			} else if !folderHas {
				if err := r.writeToMediaListFile(ctx.coll, loadedItem.FilePath); err != nil {
					return fmt.Errorf("writing to media list file: %v", err)
				}
			}

			if !dbHas {
				// the fact that this item belongs to this collection is new information.
				// save it to the collection in the DB.
				if err := r.db.saveItemToCollection(ctx.ac.account, itemID, ctx.coll.CollectionID()); err != nil {
					return fmt.Errorf("saving item to collection in DB: %v", err)
				}
			}
		}

		// check etag to see if modified remotely after it was downloaded.
		modifiedRemotely := loadedItem.ETag != ctx.item.ItemETag()
		checksum := loadedItem.Checksum
		corrupted := false

		if ctx.checkIntegrity || modifiedRemotely {
			// compare checksums; if different, file was corrupted or deleted.

			realChecksum, err := r.hash(loadedItem.FilePath)
			if err != nil {
				log.Printf("[ERROR] checking file integrity: %v", err)
				corrupted = true
			} else {
				checksum = realChecksum

				if !bytes.Equal(realChecksum, loadedItem.Checksum) {
					log.Printf("[ERROR] checksum mismatch: %s", loadedItem.FilePath)
					corrupted = true
				}
			}
		}

		if corrupted {
			log.Printf("File %s is corrupted; re-downloading", loadedItem.FilePath)
		} else if modifiedRemotely {
			Info.Printf("File %s modified remotely; re-downloading", loadedItem.FilePath)
		} else {
			return nil
		}

		if err := r.removeItem(ctx.ac.account, itemID, loadedItem.FilePath, checksum); err != nil {
			return fmt.Errorf("removing %s: %v", loadedItem.FilePath, err)
		}
	}

	it := item{
		Item:        ctx.item,
		fileName:    ctx.item.ItemName(),
		filePath:    r.repoRelative(filepath.Join(ctx.ac.account.accountPath(), ctx.coll.dirName, ctx.item.ItemName())),
		collections: map[string]struct{}{ctx.coll.CollectionID(): {}},
	}

	Info.Printf("Getting item %s: %s", it.ItemID(), it.ItemName())
	err = r.downloadAndSaveItem(ctx.ac.client, downloadingItem, it, ctx.coll, ctx.ac.account, ctx.saveEverything)
	if err != nil {
		downloadingItem.pathMu.Lock()
		downloadingItem.remove()
		downloadingItem.pathMu.Unlock()
		return fmt.Errorf("downloading and saving item: %v", err)
	}

	return nil
}

func (r *Repository) removeItem(account providerAccount, itemID string, filePath string, checksum []byte) error {
	checksumLock := r.newChecksumLocker(checksum)
	checksumLock.Lock()
	defer checksumLock.Unlock()

	var hasReferences bool

	sameItems, err := r.db.itemsWithChecksum(checksum)
	if err != nil {
		return err
	}

	for _, sameItem := range sameItems {
		if bytes.Equal(sameItem.AcctKey, account.key()) && sameItem.ItemID == itemID {
			continue
		}

		sameContent, err := r.db.loadItem(sameItem.AcctKey, sameItem.ItemID)
		if err != nil {
			return err
		}

		if sameContent.FilePath == filePath {
			hasReferences = true
			break
		}
	}

	if err := r.db.deleteItem(account.key(), itemID); err != nil {
		return err
	}

	if !hasReferences {
		Info.Printf("Removing %s", filePath)
		os.Remove(r.fullPath(filePath))
	}

	return nil
}

// reserveUniqueFilename will look in dir (which must be repo-relative)
// for targetName. If it is taken, it will change the filename by
// adding a counter to the end of it, up to a certain limit, until it
// finds an available filename. This is safe for concurrent use.
// It reserves the filename by creating it in dir, and returns the
// name of the file (or directory, depending on isDir) created in dir.
func (r *Repository) reserveUniqueFilename(dir, targetName string, isDir bool) (string, error) {
	// ensure that only one reservation takes place for this name at a time
	targetPath := filepath.Join(dir, targetName)
	channel := make(chan struct{})

	for {
		r.itemNamesMu.Lock()
		if ch, taken := r.itemNames[targetPath]; taken {
			r.itemNamesMu.Unlock()
			<-ch // wait for it to be available again
		} else {
			r.itemNames[targetPath] = channel
			r.itemNamesMu.Unlock()
			break
		}
	}
	defer func() {
		r.itemNamesMu.Lock()
		delete(r.itemNames, targetPath)
		close(channel)
		r.itemNamesMu.Unlock()
	}()

	// iterate until we find a candidate name that we can use
	candidate, candidatePath := targetName, targetPath
	for i := 2; i < 1000; i++ { // this can handle up to 1000 collisions
		candidatePath = filepath.Join(dir, candidate)
		if !r.fileExists(candidatePath) {
			break
		}
		parts := strings.SplitN(targetName, ".", 2)
		if len(parts) == 1 { // no file extension (likely a directory)
			candidate = targetName + fmt.Sprintf("-%03d", i)
			continue
		}
		candidate = strings.Join(parts, fmt.Sprintf("-%03d.", i))
	}

	finalPath := r.fullPath(candidatePath)

	if isDir {
		err := os.MkdirAll(finalPath, 0700)
		if err != nil {
			return candidate, err
		}
	} else {
		f, err := os.Create(finalPath)
		if err != nil {
			return candidate, err
		}
		f.Close()
	}

	return candidate, nil
}

// hash loads fpath (which must be repo-relative)
// and hashes it, returning the hash in bytes.
func (r *Repository) hash(fpath string) ([]byte, error) {
	f, err := os.Open(r.fullPath(fpath))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	h := sha256.New()
	_, err = io.Copy(h, f)
	if err != nil {
		return nil, err
	}

	return h.Sum(nil), nil
}

// dishonestWriter has a very niche use (unless you're a major
// news organization). It merely wraps an io.Writer so that
// if the writer tries to write to a pipe where the read end
// is closed, the function still returns a success result as
// if no error occurred. Other errors are still reported.
// (This is useful in our case when streaming data to the
// EXIF decoder as part of a MultiWriter.)
type dishonestWriter struct {
	io.Writer
}

// Write writes p to w.Writer, returning a dishonest result
// if writing fails due to a closed pipe.
func (w dishonestWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err == io.ErrClosedPipe {
		return len(p), nil
	}
	return n, err
}

type checksumLocker struct {
	repository *Repository
	checksum   string
	channel    chan struct{}
}

func (l *checksumLocker) Lock() {
	// the operations on the database are not within the same transaction,
	// so we use a map with channels to synchronize.

	l.channel = make(chan struct{})

	for {
		l.repository.itemChecksumsMu.Lock()
		if ch, taken := l.repository.itemChecksums[l.checksum]; taken {
			// another goroutine is processing the same content
			// (different item) right now; wait until it is done.
			l.repository.itemChecksumsMu.Unlock()
			<-ch
		} else {
			l.repository.itemChecksums[l.checksum] = l.channel
			l.repository.itemChecksumsMu.Unlock()
			break
		}
	}
}

func (l *checksumLocker) Unlock() {
	l.repository.itemChecksumsMu.Lock()
	delete(l.repository.itemChecksums, l.checksum)
	l.repository.itemChecksumsMu.Unlock()
	close(l.channel)
}

func (r *Repository) newChecksumLocker(checksum []byte) *checksumLocker {
	return &checksumLocker{
		repository: r,
		checksum:   hex.EncodeToString(checksum),
	}
}

func (r *Repository) downloadAndSaveItem(client Client, downloadingItem *downloadingItem, it item, coll collection, pa providerAccount, saveEverything bool) error {
	saveToMediaListFile := func(pa providerAccount, coll collection, pointedPath, itemID string) error {
		err := r.writeToMediaListFile(coll, pointedPath)
		if err != nil {
			return err
		}
		return r.db.saveItemToCollection(pa, itemID, coll.CollectionID())
	}

	itemID := it.ItemID()
	it.collections[coll.CollectionID()] = struct{}{}

	err := os.MkdirAll(r.fullPath(coll.dirPath), 0700)
	if err != nil {
		return fmt.Errorf("creating folder for collection '%s': %v", coll.CollectionName(), err)
	}

	downloadingItem.pathMu.Lock()
	itemFileName, err := r.reserveUniqueFilename(coll.dirPath, it.ItemName(), false)
	if err != nil {
		downloadingItem.pathMu.Unlock()
		return fmt.Errorf("reserving unique filename: %v", err)
	}
	it.fileName = itemFileName
	it.filePath = r.repoRelative(filepath.Join(coll.dirPath, itemFileName))
	downloadingItem.path = r.fullPath(it.filePath)
	downloadingItem.pathMu.Unlock()

	// try a few times in case of network trouble
	var h hash.Hash
	var x *exif.Exif
	var downloadErr error
	for i := 0; i < 3; i++ {
		downloadingItem.pathMu.Lock()
		outFile, err := os.Create(downloadingItem.path)
		downloadingItem.pathMu.Unlock()

		if err != nil {
			return fmt.Errorf("opening output file %s: %v", it.filePath, err)
		}

		h = sha256.New()
		pr, pw := io.Pipe()
		mw := io.MultiWriter(outFile, h, dishonestWriter{pw})

		go func() {
			// an item may not have EXIF data, and that is not
			// an error, it just means we don't have any meta
			// data from the file. if it does have EXIF data
			// and we have trouble reading it for some reason,
			// it doesn't really matter because there's nothing
			// we can do about it; so we ignore the error.
			x, _ = exif.Decode(pr)

			// the exif.Decode() call above only reads as much
			// as needed to conclude the EXIF portion, then it
			// stops reading. this is a problem, because it blocks
			// all other writes in the MultiWriter from happening
			// since this one is not reading. the DishonestWriter
			// that we wrapped the write end of the pipe with will,
			// as a special case, report a totally successful write
			// if it gets a "write to closed pipe" error. so even
			// though the whole file has likely not been read yet,
			// it is not a bug to close the read end of this pipe.
			pr.Close()
		}()

		Info.Printf("[attempt %d] Downloading %s into %s", i+1, it.ItemID(), it.filePath)
		downloadErr = client.DownloadItemInto(it.Item, mw)
		outFile.Close()
		if downloadErr == nil {
			break
		}
		log.Printf("[ERROR] downloading %s, attempt %d: %v; retrying", it.filePath, i+1, downloadErr)
	}
	if downloadErr != nil {
		return fmt.Errorf("repeatedly failed downloading %s: %v", it.filePath, downloadErr)
	}

	// I don't care about the error here. Not having EXIF data is OK.
	setting, _ := r.getSettingFromEXIF(x)

	meta := itemMeta{Setting: setting, Caption: it.ItemCaption()}
	if saveEverything {
		// NOTE: If the item caption is already stored as
		// part of the Item, this will duplicate it in
		// the database. Oh well. Hopefully it's small.
		meta.API = it.Item
	}

	dbi := &dbItem{
		ID:          itemID,
		Name:        it.ItemName(),
		FileName:    it.fileName,
		FilePath:    it.filePath,
		Meta:        meta,
		Saved:       time.Now(),
		Collections: it.collections,
		Checksum:    h.Sum(nil),
		ETag:        it.ItemETag(),
	}

	// de-duplicate at the content level: if we already have
	// an item with this checksum in the repository, point
	// to it instead of saving it again.
	checksumLock := r.newChecksumLocker(dbi.Checksum)
	checksumLock.Lock()
	defer checksumLock.Unlock()

	sameItems, err := r.db.itemsWithChecksum(dbi.Checksum)
	if err != nil {
		return fmt.Errorf("de-duplicating item '%s': %v", it.fileName, err)
	}
	for _, sameItem := range sameItems {
		sameContent, err := r.db.loadItem(sameItem.AcctKey, sameItem.ItemID)
		if err != nil {
			return err
		}

		if !bytes.Equal(sameContent.Checksum, dbi.Checksum) {
			log.Printf("[ERROR] %s has an invalid checksum index", sameItem.ItemID)
			continue
		}

		if sameContent.FilePath == dbi.FilePath {
			continue
		}

		if realChecksum, err := r.hash(sameContent.FilePath); err != nil {
			log.Printf("[ERROR] %s item points to a corrupted %q file: %v; deleting it", sameItem.ItemID, sameContent.FilePath, err)
			if err := r.db.deleteItem(sameItem.AcctKey, sameItem.ItemID); err != nil {
				return err
			}
			continue
		} else if !bytes.Equal(realChecksum, sameContent.Checksum) {
			log.Printf("[ERROR] %s item points to %q file with a different checksum; deleting it", sameItem.ItemID, sameContent.FilePath)
			if err := r.db.deleteItem(sameItem.AcctKey, sameItem.ItemID); err != nil {
				return err
			}
			continue
		}

		// this content is not unique; it exists elsewhere in the repo.
		// save this item to this collection, but we'll delete the
		// hard copy of the file we just downloaded since we'll point
		// to where it already exists in the repository.

		// delete the physical copy we just downloaded
		downloadingItem.pathMu.Lock()
		downloadingItem.remove()
		downloadingItem.pathMu.Unlock()

		Info.Printf("The content of item %s already exists in repository as %q; de-duplicating", it.ItemID(), sameContent.FilePath)
		dbi.FilePath = sameContent.FilePath

		// write that item's path to the media list file for this item
		if err = saveToMediaListFile(pa, coll, sameContent.FilePath, itemID); err != nil {
			return err
		}

		break
	}

	downloadingItem.pathMu.Lock()

	// we've got everything on disk that we need,
	// now commit this item to the database!
	if err := r.db.saveItem(pa.key(), itemID, dbi); err != nil {
		downloadingItem.remove() // no record of it in the database, so don't keep it on disk...
		downloadingItem.pathMu.Unlock()
		return fmt.Errorf("saving item '%s' to database: %v", it.fileName, err)
	} else {
		downloadingItem.path = ""
		downloadingItem.pathMu.Unlock()
		Info.Printf("Committed item '%s' to disk and database", it.fileName)
		return nil
	}
}

// accountItem is used to identify an item across
// any account in the repository; used for checksums
// and repository-wide de-duplication.
type accountItem struct {
	AcctKey []byte
	ItemID  string
}

// repoRelative turns a full path into a path that
// is relative to the repository root. Paths stored
// in the database or shown in media list files should
// always be repo-relative; only switch to full paths
// (or "relative to current directory" paths) when
// interacting with the file system.
func (r *Repository) repoRelative(fpath string) string {
	return strings.TrimPrefix(fpath, filepath.Clean(r.path)+string(filepath.Separator))
}

// fullPath converts a repo-relative path to a full path
// usable with the file system. Paths should always be stored
// as repo-relative, but must be converted to their "full"
// (or, more precisely, "absolute or relative to current
// directory") path for interaction with the file system.
func (r *Repository) fullPath(repoRelative string) string {
	return filepath.Join(r.path, repoRelative)
}

// getSettingFromEXIF extracts coordinate, timestamp, and
// altitude information from x.
func (r *Repository) getSettingFromEXIF(x *exif.Exif) (*setting, error) {
	if x == nil {
		return nil, nil
	}

	// coordinates
	lat, lon, err := x.LatLong()
	if err != nil {
		return nil, fmt.Errorf("getting coordinates from EXIF: %v", err)
	}

	// timestamp
	ts, err := x.DateTime()
	if err != nil {
		return nil, fmt.Errorf("getting timestamp from EXIF: %v", err)
	}

	// altitude
	rawAlt, err := x.Get(exif.GPSAltitude)
	if err != nil {
		return nil, fmt.Errorf("getting altitude from EXIF: %v", err)
	}
	alt, err := rawAlt.Rat(0)
	if err != nil {
		return nil, fmt.Errorf("converting altitude value: %v", err)
	}
	altFlt, _ := alt.Float64()

	// altitude reference, adjust altitude if needed
	altRef, err := x.Get(exif.GPSAltitudeRef)
	if err != nil {
		return nil, fmt.Errorf("getting altitude reference from EXIF: %v", err)
	}
	altRefInt, err := altRef.Int(0)
	if err != nil {
		return nil, fmt.Errorf("converting altitude reference: %v", err)
	}
	if altRefInt == 1 && altFlt > 0 {
		// 0 indicates above sea level, 1 is below sea level.
		// we expect the altitude relative to sea level.
		altFlt *= -1.0
	}

	return &setting{
		Latitude:   lat,
		Longitude:  lon,
		OriginTime: ts,
		Altitude:   altFlt,
	}, nil
}

// localCollectionHasItemOnDisk returns true if the given collection
// has the item in it, either as an actual file or a reference
// in the media list file.
func (r *Repository) localCollectionHasItemOnDisk(pa providerAccount, coll collection, localItem *dbItem) (bool, error) {
	// check for item on disk first
	if r.fileExists(filepath.Join(coll.dirPath, localItem.FileName)) {
		return true, nil
	}

	// check others.txt file to see if item is in the list
	return r.mediaListHasItem(coll.dirPath, localItem)
}

// fileExists returns true if there is not an
// error stat'ing the file at fpath, which will
// be evaluated relative to the repo path.
func (r *Repository) fileExists(fpath string) bool {
	_, err := os.Stat(r.fullPath(fpath))
	return err == nil
}

// accountClient is a providerAccount with
// a Client authorized to access the account.
type accountClient struct {
	account providerAccount
	client  Client
}
