// 基于哈希分区的KV嵌入式数据库
// 索引文件结构：数据0文件偏移量(5) 数据0列表分配大小(2 buckets) 数据0列表真实长度(3)
// 数据文件0结构1：[数据项长度(10bit) 键值分配长度(12bit buckets) 键值真实长度(2) 数据项类型(2bit - 0) 键值(变长,最大5) 键名(变长)](变长,链表)
// 数据文件0结构2：[数据项长度(10bit) 键值分配长度(12bit buckets) 键值真实长度(2) 数据项类型(2bit - 1|2|3) 数据文件偏移量(5) 键名(变长)](变长,链表)
// 数据文件1结构 ：键值(变长)
// 数据项类型 :
// 0: 数据文件0中的数据项键值放在第5项中，最大长度为5byte
// 1-3: 数据文件0中的数据项键值放在数据文件1中，第5项为数据文件1中的索引位置，键值cap存放gBUCKET_SIZE的倍数

package gkvdb

import (
    "os"
    "g/os/gfile"
    "strings"
    "g/encoding/gbinary"
    "g/os/gfilepool"
    "errors"
    "g/encoding/ghash"
    "bytes"
    "strconv"
)

const (
    gPARTITION_SIZE          = 1048576                    // 哈希表分区大小(大小约为10MB)
    gMAX_KEY_SIZE            = (0xFFFF >> 6) - 10         // 键名最大长度(1013)
    gMAX_VALUE_SIZE          = 0xFFFF                     // 键值最大长度(65535)
    gBUCKET_SIZE             = 64                         // 数据文件0文件列表分块大小(byte, 值越大，初始化时占用的空间越大)
    gFILE_POOL_CACHE_TIMEOUT = 60                         // 文件指针池缓存时间(秒)
)

// KV数据库
type DB struct {
    path   string          // 数据文件存放目录路径
    prefix string          // 数据文件名前缀
    ixfp   *gfilepool.Pool // 索引文件打开指针池(用以高并发下的IO复用)
    db0fp  *gfilepool.Pool // 数据文件0打开指针池(包含索引信息和部分数据信息)
    db1fp  *gfilepool.Pool // 数据文件1打开指针池
}

// KV数据检索记录
type Record struct {
    hash64    uint64  // 64位的hash code
    part      int64   // 分区位置
    key       []byte  // 键名
    value     []byte  // 键值(当键值<=5时直接存放到db0文件中，检索时便能直接获取到值)
    ix struct {
        start int64   // 一级索引开始位置
        end   int64   // 一级索引结束位置
    }
    db0 struct {
        start  int64  // 开始位置
        end    int64  // 结束位置
        cap    int    // 分配长度(byte)
        size   int    // 真实长度(byte)
        buffer []byte // 数据项列表([]byte)
        index  int    // 列表匹配的索引位置
        match  bool   // 是否在查找中准确匹配key
    }
    db1 struct {
        start  int64  // 数据文件中的开始地址
        end    int64  // 数据文件中的结束地址
        vcap   uint   // 键值允许存放的的最大长度（用以修改对比）
        klen   uint   // 键名大小
        vlen   uint   // 键值大小(byte)
        vtype  uint   // 键值类型
    }
}

// 创建一个KV数据库
func New(path, prefix string) (*DB, error) {
    path = strings.TrimRight(path, gfile.Separator)
    if prefix == "" {
        prefix = "gkvdb"
    }
    if !gfile.Exists(path) {
        if err := gfile.Mkdir(path); err != nil {
            return nil, err
        }
    }
    // 目录权限检测
    if !gfile.IsWritable(path) {
        return nil, errors.New(path + " is not writable")
    }
    // 索引/数据文件权限检测
    ixpath  := path + gfile.Separator + prefix + ".ix"
    db0path := path + gfile.Separator + prefix + ".db0"
    db1path := path + gfile.Separator + prefix + ".db1"
    if gfile.Exists(ixpath) && (!gfile.IsWritable(ixpath) || !gfile.IsReadable(ixpath)){
        return nil, errors.New("permission denied to index file: " + ixpath)
    }
    if gfile.Exists(db0path) && (!gfile.IsWritable(db0path) || !gfile.IsReadable(db0path)){
        return nil, errors.New("permission denied to 0 data file: " + db0path)
    }
    if gfile.Exists(db1path) && (!gfile.IsWritable(db1path) || !gfile.IsReadable(db1path)){
        return nil, errors.New("permission denied to 1 data file: " + db1path)
    }
    // 创建文件指针池
    ixfp  := gfilepool.New(ixpath,  os.O_RDWR|os.O_CREATE, gFILE_POOL_CACHE_TIMEOUT)
    db0fp := gfilepool.New(db0path, os.O_RDWR|os.O_CREATE, gFILE_POOL_CACHE_TIMEOUT)
    db1fp := gfilepool.New(db1path, os.O_RDWR|os.O_CREATE, gFILE_POOL_CACHE_TIMEOUT)
    return &DB {
        path   : path,
        prefix : prefix,
        ixfp   : ixfp,
        db0fp  : db0fp,
        db1fp  : db1fp,
    }, nil
}

// 计算关键字的hash code
func (db *DB) getHash(key []byte) uint64 {
    return ghash.BKDRHash64(key)
}

// 计算关键字再一级索引文件中的偏移量
func (db *DB) getPartitionByHash64(hash uint64) int64 {
    return int64(hash%gPARTITION_SIZE)
}

// 获得一级索引信息
func (db *DB) getIndexInfoByRecord(record *Record) error {
    pf, err := db.ixfp.File()
    if err != nil {
        return err
    }
    defer pf.Close()
    record.ix.start = record.part*10
    record.ix.end   = record.ix.start + 10
    if buffer := gfile.GetBinContentByTwoOffsets(pf.File(), record.ix.start, record.ix.end); buffer != nil {
        //fmt.Println("get index:",buffer)
        record.db0.start = gbinary.DecodeToInt64(buffer[0:5])
        record.db0.cap   = int(gbinary.DecodeToUint16(buffer[5:7])*gBUCKET_SIZE)
        record.db0.size  = int(gbinary.DecodeToUint32(buffer[7:10]))
        record.db0.end   = record.db0.start + int64(record.db0.size)
        return nil
    }
    return nil
}

// 获得数据检索信息
func (db *DB) getDataInfoByRecord(record *Record) error {
    pf, err := db.db0fp.File()
    if err != nil {
        return err
    }
    defer pf.Close()
    record.db0.buffer = gfile.GetBinContentByTwoOffsets(pf.File(), record.db0.start, record.db0.end)
    if record.db0.buffer != nil {
        //fmt.Println("get record", record)
        // 线性查找
        for i := 0; i < len(record.db0.buffer); {
            buffer := record.db0.buffer[i:]
            bits   := gbinary.DecodeBytesToBits(buffer[0:5])
            length := gbinary.DecodeBits(bits[0 : 10])
            key    := buffer[10 : length]
            if bytes.Compare(key, record.key) == 0 {
                record.db0.index  = i
                record.db0.match  = true
                record.db1.klen   = length - 10
                record.db1.vcap   = gbinary.DecodeBits(bits[10 : 22])*gBUCKET_SIZE
                record.db1.vlen   = gbinary.DecodeBits(bits[22 : 38])
                record.db1.vtype  = gbinary.DecodeBits(bits[38 : 40])
                if record.db1.vtype == 0 {
                    record.value = buffer[5 : 5 + record.db1.vlen]
                } else {
                    record.db1.start = gbinary.DecodeToInt64(buffer[5 : 10])
                    record.db1.end   = record.db1.start + int64(record.db1.vlen)
                }
                break
            } else {
                i += int(length)
            }
        }
    }
    return nil
}

// 查询检索信息
func (db *DB) getRecordByKey(key []byte) (*Record, error) {
    hash64 := db.getHash(key)
    part   := db.getPartitionByHash64(hash64)
    record := &Record {
        hash64  : hash64,
        part    : part,
        key     : key,
    }

    // 查询索引信息
    if err := db.getIndexInfoByRecord(record); err != nil {
        return record, err
    }

    // 查询数据信息
    if record.db0.end > 0 {
        if err := db.getDataInfoByRecord(record); err != nil {
            return record, err
        }
    }
    return record, nil
}

// 查询数据信息键值
func (db *DB) getValueByKey(key []byte) ([]byte, error) {
    record, err := db.getRecordByKey(key)
    if err != nil {
        return nil, err
    }

    if record == nil {
        return nil, nil
    }

    if len(record.value) > 0 {
        return record.value, nil
    }

    if record.db1.end > 0 {
        pf, err := db.db1fp.File()
        if err != nil {
            return nil, err
        }
        defer pf.Close()
        buffer := gfile.GetBinContentByTwoOffsets(pf.File(), record.db1.start, record.db1.end)
        if buffer != nil {
            return buffer, nil
        }
    }
    return nil, nil
}

// 关闭数据库链接
func (db *DB) Close() {
    db.ixfp.Close()
    db.db0fp.Close()
    db.db1fp.Close()
}

// 删除数据库
func (db *DB) Remove(sure bool) {
    if sure {
        db.Close()
        gfile.Remove(db.path)
    }
}

// 查询KV数据
func (db *DB) Get(key []byte) []byte {
    value, _ := db.getValueByKey(key)
    return value
}


// 设置KV数据
func (db *DB) Set(key []byte, value []byte) error {
    if len(key) > gMAX_KEY_SIZE {
        return errors.New("too large key size, max allowed: " + strconv.Itoa(gMAX_KEY_SIZE))
    }
    if len(key) > gMAX_VALUE_SIZE {
        return errors.New("too large value size, max allowed: " + strconv.Itoa(gMAX_VALUE_SIZE))
    }

    record, err := db.getRecordByKey(key)
    if err != nil {
        return err
    }
     //fmt.Println(record)
    //return nil
    // 写入数据文件，并更新record信息
    if err := db.insertDataByRecord(key, value, record); err != nil {
        return err
    }
    //oldcap := record.db0.cap
    // 根据record信息更新索引文件
    if err := db.createIndexByRecord(record); err != nil {
        return err
    }
    //if record.db0.cap != oldcap {
    //    if record.db0.cap > gINDEX1_BUCKET_SIZE {
    //        fmt.Printf("new cap %d for key: %v\n", record.db0.cap, string(key))
    //    }
    //}
    return nil
}

// 插入一条KV数据
func (db *DB) insertDataByRecord(key []byte, value []byte, record *Record) error {
    db0pf, err := db.db0fp.File()
    if err != nil {
        return err
    }
    defer db0pf.Close()
    bits   := make([]uint8, 0)
    data   := make([]byte, 0)
    buffer := make([]byte, 0)
    record.db1.klen = uint(len(key))
    record.db1.vlen = uint(len(value))
    // 如果键值大于5byte, 写入到db1中
    if len(value) > 5 {
        db1pf, err := db.db1fp.File()
        if err != nil {
            return err
        }
        defer db1pf.Close()
        // 判断是否额外分配键值存储空间
        if record.db1.end <= 0 || record.db1.vcap < record.db1.vlen {
            // @todo 碎片管理
            start, err := db1pf.File().Seek(0, 2)
            if err != nil {
                return err
            }
            record.db1.start = start
            record.db1.end   = start + int64(record.db1.vlen)
        }
        // 键值大小必须为gBUCKET_SIZE的整数倍
        vbuffer := make([]byte, 0)
        vbuffer  = append(vbuffer, value...)
        if record.db1.vcap < record.db1.vlen {
            for {
                record.db1.vcap += gBUCKET_SIZE
                if record.db1.vcap >= record.db1.vlen {
                    break
                }
            }
            for i := 0; i < int(record.db1.vcap - record.db1.vlen); i++ {
                vbuffer = append(vbuffer, byte(0))
            }
        }
        if _, err = db1pf.File().WriteAt(vbuffer, record.db1.start); err != nil {
            return err
        }
        // 改变value的值为db1文件偏移地址
        value            = gbinary.EncodeUint64(uint64(record.db1.start))[0:5]
        record.db1.vtype = 1
    }
    // 二进制打包
    bits = gbinary.EncodeBits(bits, record.db1.klen + 10, 10)
    bits = gbinary.EncodeBits(bits, record.db1.vcap/gBUCKET_SIZE, 12)
    bits = gbinary.EncodeBits(bits, record.db1.vlen, 16)
    bits = gbinary.EncodeBits(bits, record.db1.vtype, 2)
    data = append(data, gbinary.EncodeBitsToBytes(bits)...)
    data = append(data, value...)
    for i := 0; i < 5 - len(value); i++ {
        data  = append(data, byte(0))
    }
    data = append(data, key...)
    //fmt.Println("data:", data)
    // 数据列表打包
    buffer = append(buffer, data...)
    if len(record.db0.buffer) > 0 {
        if record.db0.match {
            buffer = append(buffer, record.db0.buffer[0 : record.db0.index]...)
            buffer = append(buffer, record.db0.buffer[record.db0.index + 10 + int(record.db1.klen) :]...)
        } else {
            buffer = append(buffer, record.db0.buffer...)
        }
    }

    //fmt.Println("record:", record)
    //fmt.Println("db0   buffer:", record.db0.buffer)
    //fmt.Println("write buffer:", buffer)

    // 判断数据列表空间是否足够
    record.db0.size = len(buffer)
    if record.db0.cap < record.db0.size {
        // @todo 碎片管理
        start, err := db0pf.File().Seek(0, 2)
        if err != nil {
            return err
        }
        // 每次分配必须为gBUCKET_SIZE
        for {
            record.db0.cap += gBUCKET_SIZE
            if record.db0.cap >= record.db0.size {
                break
            }
        }
        record.db0.start = start
        record.db0.end   = start + int64(record.db0.cap)
        for i := 0; i < int(record.db0.cap - record.db0.size); i++ {
            buffer = append(buffer, byte(0))
        }
    }

    if _, err = db0pf.File().WriteAt(buffer, record.db0.start); err != nil {
        return err
    }
    return nil
}

// 根据record重新创建索引信息
func (db *DB) createIndexByRecord(record *Record) error {
    ixpf, err := db.ixfp.File()
    if err != nil {
        return err
    }
    defer ixpf.Close()
    buffer  := make([]byte, 0)
    db0cap  := uint16(record.db0.cap/gBUCKET_SIZE)
    db0size := uint32(record.db0.size)
    buffer   = append(buffer, gbinary.EncodeInt64(record.db0.start)[0:5]...)
    buffer   = append(buffer, gbinary.EncodeUint16(db0cap)...)
    buffer   = append(buffer, gbinary.EncodeUint32(db0size)[0:3]...)
    //fmt.Println("create:", buffer)
    if _, err = ixpf.File().WriteAt(buffer, record.part*10); err != nil {
        return err
    }
    return nil
}


