package main

import (
    "io"
    "os"
    "os/exec"
    "fmt"
    "time"
    "sync"
    "net/http"
    "crypto/sha256"
    "encoding/json"
    "code.google.com/p/go-uuid/uuid"
    "github.com/garyburd/redigo/redis"
)

type PackagePort struct {
    PkgId string
    PkgFpath string
    Event *sync.Cond
    Err error
    Size int64
}
type PackageTransport struct {
    Port map[string]*PackagePort
    Lock *sync.Mutex
    CRsPl *redis.Pool
    LRsPl *redis.Pool
}
type PackageReader struct {
    PkgFile *os.File
    Port *PackagePort
}
/*
    Package tranport reader
*/
func (reader PackageReader) Read(buf []byte) (int,error) {
    var retlen int
    var reterr error

    oldsize := reader.Port.Size
    retlen,reterr = reader.PkgFile.Read(buf)
    if retlen == 0 && reterr == io.EOF {
	reader.Port.Event.L.Lock()

	if oldsize == reader.Port.Size && reader.Port.Err == nil {
	    reader.Port.Event.Wait()
	}

	reader.Port.Event.L.Unlock()

	if oldsize < reader.Port.Size {
	    retlen,reterr = reader.PkgFile.Read(buf)
	} else {
	    retlen = 0
	    reterr = reader.Port.Err
	}
    }

    return retlen,reterr
}
func (reader PackageReader) Close() error {
    return reader.PkgFile.Close()
}

type Package struct {
    PkgId string    `json:"-"`
    ApiId string    `json:"apiid,omitempty"`
    When int64	    `json:"when,omitempty"`
    Expire int64    `json:"expire"`
    Env *APIEnv	    `json:"-"`
}
type ErrPackageMiss struct {
    PkgId string
}
func (err ErrPackageMiss) Error() string {
    return err.PkgId
}
type ErrPackageHit struct {
    PkgId string
}
func (err ErrPackageHit) Error() string {
    return err.PkgId
}
type ErrPackageAccess struct {
    PkgId string
}
func (err ErrPackageAccess) Error() string {
    return err.PkgId
}
/*
    Try to get the package which has been stored at local.
*/
func (pkg *Package) Get() error {
    var err error

    metabuf,err := redis.Bytes(
	pkg.Env.LRs.Do("HGET","PACKAGE@" + pkg.PkgId,"meta"))
    if err != nil {
	return ErrPackageMiss{pkg.PkgId}
    }
    if err := json.Unmarshal(metabuf,&pkg); err != nil {
	return err
    }

    return nil
}
/*
    Import new package from uploaded file.
    This function will add the package to the cluster, primarily used by
    API add_pkg.
*/
func (pkg *Package) Import(pkgfpath string) error {
    pkgdpath,err := decompress(pkg.PkgId,pkgfpath)
    if err != nil {
	return err
    }
    if err := loadMeta(pkg,pkgdpath); err != nil {
	os.RemoveAll(pkgdpath)
	return err
    }
    pkg.ApiId = pkg.Env.ApiId
    pkg.When = time.Now().Unix()

    if err := modifyMeta(pkg,pkgdpath); err != nil {
	os.RemoveAll(pkgdpath)
	return err
    }
    if _,err := compress(pkg.PkgId,pkgdpath); err != nil {
	os.RemoveAll(pkgdpath)
	return err
    }
    if err := updatePackage(pkg); err != nil {
	os.RemoveAll(pkgdpath)
	return err
    }

    return nil
}
/*
    Export nginx internal url.
*/
func (pkg *Package) Export() string {
    return "/package/" + pkg.PkgId + ".tar.xz"
}
/*
    Transport package from other node.
*/
func (pkg *Package) Transport() (io.ReadCloser,error) {
    reader,err := tranportPackage(pkg.PkgId,pkg.Env.PkgTran,pkg.Env.CRs)
    return reader,err
}

/*
    Generate new PkgId.
*/
func PackageGenID() string {
    return fmt.Sprintf("%x",sha256.Sum256([]byte(uuid.NewUUID().String())))
}
/*
    Just a helper which can create an empty package.
*/
func PackageCreate(PkgId string,env *APIEnv) *Package {
    return &Package{
	PkgId:PkgId,
	ApiId:"",
	When:0,
	Expire:0,
	Env:env,
    }
}
/*
    TBD
*/
func PackageClean(PkgId string,env *APIEnv) error {
    env.LRs.Do("DEL","PACKAGE@" + PkgId)
    env.CRs.Do("SREM","PKG_NODE@" + PkgId,BIND)
    return nil
}
/*
    Transport Core loop
*/

/*
    Compress package to SOTRAGE_PATH/package/PkgId.tar.xz
*/
func compress(PkgId string,dpath string) (string,error) {
    fpath := STORAGE_PATH + "/package/" + PkgId + ".tar.xz"
    cmd := exec.Command(TAR_PATH,"-Jcf",fpath,"-C",dpath,".")
    if err := cmd.Run(); err != nil {
	return "",err
    }

    return fpath,nil
}
/*
    Decompress pkg.tar.xz to SOTRAGE_PATH/package/PkgId.
*/
func decompress(PkgId string,fpath string) (string,error) {
    dpath := STORAGE_PATH + "/package/" + PkgId
    if err := os.Mkdir(dpath,0700); err != nil {
	return "",err
    }
    cmd := exec.Command(TAR_PATH,"-Jxf",fpath,"-C",dpath)
    if err := cmd.Run(); err != nil {
	os.RemoveAll(dpath)
	return "",err
    }

    return dpath,nil
}
/*
    Load metadata from meta.json.
*/
func loadMeta(pkg *Package,dpath string) error {
    metafile,err := os.Open(dpath + "/meta.json")
    if err != nil {
	os.RemoveAll(dpath)
	return err
    }
    metabuf := make([]byte,PKGMETA_MAXSIZE)
    metalen,err := metafile.Read(metabuf);
    metafile.Close()
    if err != nil {
	os.RemoveAll(dpath)
	return err
    }
    if err := json.Unmarshal(metabuf[:metalen],pkg); err != nil {
	os.RemoveAll(dpath)
	return err
    }

    return nil
}
/*
    Modify metadata to meta.json.
*/
func modifyMeta(pkg *Package,dpath string) error {
    var meta map[string]interface{}

    metafile,err := os.OpenFile(dpath + "/meta.json",os.O_RDWR,0644)
    if err != nil {
	return err
    }
    defer metafile.Close()

    readbuf := make([]byte,PKGMETA_MAXSIZE)
    metalen,err := metafile.Read(readbuf);
    if err != nil {
	return err
    }
    if err := json.Unmarshal(readbuf[:metalen],&meta); err != nil {
	return err
    }

    meta["pkgid"] = pkg.PkgId
    meta["apiid"] = pkg.ApiId
    meta["when"] = pkg.When
    meta["expire"] = pkg.Expire

    writebuf,err := json.MarshalIndent(meta,"","\t")
    if err != nil {
	return err
    }
    _,err = metafile.WriteAt(writebuf,0)
    if err != nil {
	return err
    }

    return nil
}
/*
    Set package metadata, update database.
*/
func updatePackage(pkg *Package) error {
    metabuf,_ := json.Marshal(pkg)
    pkg.Env.LRs.Do("HSET","PACKAGE@" + pkg.PkgId,"meta",metabuf)
    pkg.Env.CRs.Do("SADD","PKG_NODE@" + pkg.PkgId,BIND)
    pkg.Env.LRs.Do("EXPIREAT","PACKAGE@" + pkg.PkgId,pkg.Expire)
    pkg.Env.CRs.Do("EXPIREAT","PKG_NODE@" + pkg.PkgId,pkg.Expire)

    return nil
}
func tranportPackage(
    pkgid string,
    pkgtran *PackageTransport,
    lrs redis.Conn,
) (PackageReader,error) {
    var reterr error

    reader := PackageReader{}
    tranflag := false
    reterr = nil

    pkgtran.Lock.Lock()

    port,ok := pkgtran.Port[pkgid]
    if !ok {
	exist,err := redis.Int(lrs.Do("EXISTS","PACKAGE@" + pkgid))
	if err != nil {
	    reterr = err
	} else if exist == 1 {
	    reterr = ErrPackageHit{pkgid}
	} else {
	    port = &PackagePort{
		PkgId:pkgid,
		PkgFpath:STORAGE_PATH + "/package/" + pkgid + ".tar.xz",
		Event:sync.NewCond(&sync.Mutex{}),
		Err:nil,
		Size:0,
	    }
	    if _,err := os.Create(port.PkgFpath); err != nil {
		reterr = err
	    } else {
		pkgtran.Port[pkgid] = port
		tranflag = true
	    }
	}
    }

    pkgtran.Lock.Unlock()

    if reterr != nil {
	return reader,reterr
    }

    if tranflag {
	go func(port *PackagePort,pkgtran *PackageTransport) {
	    err := doTransport(port,pkgtran)

	    pkgtran.Lock.Lock()

	    if err != nil {
		os.Remove(port.PkgFpath)
	    }
	    delete(pkgtran.Port,port.PkgId)

	    pkgtran.Lock.Unlock()

	    if err != nil {
		port.Event.L.Lock()

		port.Err = err

		port.Event.L.Unlock()
		port.Event.Broadcast()
	    }
	}(port,pkgtran)
    }

    reader.PkgFile,_ = os.Open(port.PkgFpath)
    reader.Port = port

    return reader,nil
}
func doTransport(port *PackagePort,pkgtran *PackageTransport) error {
    var err error

    crs := pkgtran.CRsPl.Get()
    crs.Do("SELECT",1)

    bind,err := redis.String(
	crs.Do("SRANDMEMBER","PKG_NODE@" + port.PkgId))
    if err != nil {
	return err
    }
    url := fmt.Sprintf(
	"http://%s/capi/%s/tran_pkg/%s",
	bind,
	APIKEY,
	port.PkgId,
    )
    tran_res,err := http.Get(url)
    if err != nil {
	return err
    }
    defer tran_res.Body.Close()

    pkgfile,err := os.OpenFile(port.PkgFpath,os.O_WRONLY,0644)
    if err != nil {
	return err
    }
    defer pkgfile.Close()

    buf := make([]byte,65536)
    for {
	retlen,err := tran_res.Body.Read(buf)
	retlen,err = pkgfile.Write(buf[:retlen])
	if retlen == 0 && err == nil {
	    err = io.EOF
	}

	port.Event.L.Lock()

	port.Err = err
	port.Size += int64(retlen)

	port.Event.L.Unlock()
	port.Event.Broadcast()

	if err != nil {
	    break
	}
    }
    if err != io.EOF {
	return err
    }

    /*
    pkgdpath,err := decompress(port.PkgId,pkgfpath)
    if err != nil {
	os.Remove(pkgfpath)
	return err
    }

    if err := loadMeta(pkg,pkgdpath); err != nil {
	os.Remove(pkgfpath)
	os.RemoveAll(pkgdpath)
	return err
    }
    if err := updatePackage(pkg); err != nil {
	os.Remove(pkgfpath)
	os.RemoveAll(pkgdpath)
	return err
    }
    */

    return nil
}
