package wire

import (
	binary "encoding/binary"
	"fmt"

	ssz "github.com/OffchainLabs/methodical-ssz/ssz"
)

func (c *PingRequest) SizeSSZ() int {
	size := 37
	size += len(c.RootTip)
	return size
}

func (c *PingRequest) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *PingRequest) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error
	offset := 37

	// Field 0: ChainID
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[0])
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[1])
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[2])
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[3])

	// Field 1: HasRootTip
	if c.HasRootTip {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}

	// Field 2: RootTip
	dst = ssz.WriteOffset(dst, offset)
	offset += len(c.RootTip)

	// Field 2: RootTip
	if len(c.RootTip) > 4294967295 {
		return nil, ssz.ErrListTooBig
	}
	dst = append(dst, c.RootTip...)
	return dst, err
}

func (c *PingRequest) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size < 37 {
		return ssz.ErrSize
	}

	sszSlice0 := buf[0:32]  // c.ChainID
	sszSlice1 := buf[32:33] // c.HasRootTip

	sszVarOffset2 := ssz.ReadOffset(buf[33:37]) // c.RootTip
	if sszVarOffset2 != 37 {
		return ssz.ErrInvalidVariableOffset
	}
	if sszVarOffset2 > size {
		return ssz.ErrOffset
	}
	sszSlice2 := buf[sszVarOffset2:] // c.RootTip

	// Field 0: ChainID
	c.ChainID[0] = binary.LittleEndian.Uint64(sszSlice0[0:8])
	c.ChainID[1] = binary.LittleEndian.Uint64(sszSlice0[8:16])
	c.ChainID[2] = binary.LittleEndian.Uint64(sszSlice0[16:24])
	c.ChainID[3] = binary.LittleEndian.Uint64(sszSlice0[24:32])

	// Field 1: HasRootTip
	if sszSlice1[0] > 1 {
		return ssz.ErrInvalidSerialization
	}
	if sszSlice1[0] == 1 {
		c.HasRootTip = true
	} else {
		c.HasRootTip = false
	}

	// Field 2: RootTip
	c.RootTip = append([]byte{}, Data(sszSlice2)...)
	return err
}

func (c *PingRequest) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *PingRequest) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: ChainID
	hh.AppendUint64(c.ChainID[0])
	hh.AppendUint64(c.ChainID[1])
	hh.AppendUint64(c.ChainID[2])
	hh.AppendUint64(c.ChainID[3])
	// Field 1: HasRootTip
	hh.PutBool(c.HasRootTip)
	// Field 2: RootTip

	{
		if len(c.RootTip) > 4294967295 {
			return ssz.ErrBytesLength
		}
		subIndx := hh.Index()
		hh.AppendBytes32(c.RootTip)
		numItems := uint64(len(c.RootTip))
		hh.MerkleizeWithMixin(subIndx, numItems, (4294967295*1+31)/32)
	}

	hh.Merkleize(indx)
	return nil
}

func (c *PongResponse) SizeSSZ() int {
	size := 32

	return size
}

func (c *PongResponse) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *PongResponse) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error

	// Field 0: ChainID
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[0])
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[1])
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[2])
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[3])

	return dst, err
}

func (c *PongResponse) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size != 32 {
		return ssz.ErrSize
	}

	sszSlice0 := buf[0:32] // c.ChainID

	// Field 0: ChainID
	c.ChainID[0] = binary.LittleEndian.Uint64(sszSlice0[0:8])
	c.ChainID[1] = binary.LittleEndian.Uint64(sszSlice0[8:16])
	c.ChainID[2] = binary.LittleEndian.Uint64(sszSlice0[16:24])
	c.ChainID[3] = binary.LittleEndian.Uint64(sszSlice0[24:32])
	return err
}

func (c *PongResponse) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *PongResponse) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: ChainID
	hh.AppendUint64(c.ChainID[0])
	hh.AppendUint64(c.ChainID[1])
	hh.AppendUint64(c.ChainID[2])
	hh.AppendUint64(c.ChainID[3])
	hh.Merkleize(indx)
	return nil
}

func (c *SlaveInfo) SizeSSZ() int {
	size := 38
	size += len(c.Host)
	return size
}

func (c *SlaveInfo) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *SlaveInfo) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error
	offset := 38

	// Field 0: Host
	dst = ssz.WriteOffset(dst, offset)
	offset += len(c.Host)

	// Field 1: Port
	dst = binary.LittleEndian.AppendUint16(dst, c.Port)

	// Field 2: ChainID
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[0])
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[1])
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[2])
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[3])

	// Field 0: Host
	if len(c.Host) > 4294967295 {
		return nil, ssz.ErrListTooBig
	}
	dst = append(dst, c.Host...)
	return dst, err
}

func (c *SlaveInfo) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size < 38 {
		return ssz.ErrSize
	}

	sszSlice1 := buf[4:6]  // c.Port
	sszSlice2 := buf[6:38] // c.ChainID

	sszVarOffset0 := ssz.ReadOffset(buf[0:4]) // c.Host
	if sszVarOffset0 != 38 {
		return ssz.ErrInvalidVariableOffset
	}
	if sszVarOffset0 > size {
		return ssz.ErrOffset
	}
	sszSlice0 := buf[sszVarOffset0:] // c.Host

	// Field 0: Host
	c.Host = append([]byte{}, sszSlice0...)

	// Field 1: Port
	c.Port = binary.LittleEndian.Uint16(sszSlice1)

	// Field 2: ChainID
	c.ChainID[0] = binary.LittleEndian.Uint64(sszSlice2[0:8])
	c.ChainID[1] = binary.LittleEndian.Uint64(sszSlice2[8:16])
	c.ChainID[2] = binary.LittleEndian.Uint64(sszSlice2[16:24])
	c.ChainID[3] = binary.LittleEndian.Uint64(sszSlice2[24:32])
	return err
}

func (c *SlaveInfo) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *SlaveInfo) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: Host

	{
		if len(c.Host) > 4294967295 {
			return ssz.ErrBytesLength
		}
		subIndx := hh.Index()
		hh.AppendBytes32(c.Host)
		numItems := uint64(len(c.Host))
		hh.MerkleizeWithMixin(subIndx, numItems, (4294967295*1+31)/32)
	}

	// Field 1: Port
	hh.PutUint16(c.Port)
	// Field 2: ChainID
	hh.AppendUint64(c.ChainID[0])
	hh.AppendUint64(c.ChainID[1])
	hh.AppendUint64(c.ChainID[2])
	hh.AppendUint64(c.ChainID[3])
	hh.Merkleize(indx)
	return nil
}

func (c *ConnectToSlavesRequest) SizeSSZ() int {
	size := 4
	for _, o := range c.SlaveInfoList {
		size += 4
		size += o.SizeSSZ()
	}
	return size
}

func (c *ConnectToSlavesRequest) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *ConnectToSlavesRequest) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error
	offset := 4

	// Field 0: SlaveInfoList
	dst = ssz.WriteOffset(dst, offset)
	for _, o := range c.SlaveInfoList {
		offset += 4
		offset += o.SizeSSZ()
	}

	// Field 0: SlaveInfoList
	if len(c.SlaveInfoList) > 4294967295 {
		return nil, ssz.ErrListTooBig
	}
	{
		offset = 4 * len(c.SlaveInfoList)
		for _, o := range c.SlaveInfoList {
			dst = ssz.WriteOffset(dst, offset)
			offset += o.SizeSSZ()
		}
	}
	for _, o := range c.SlaveInfoList {
		if dst, err = o.MarshalSSZTo(dst); err != nil {
			return nil, fmt.Errorf("SlaveInfoList: %w", err)
		}
	}
	return dst, err
}

func (c *ConnectToSlavesRequest) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size < 4 {
		return ssz.ErrSize
	}

	sszVarOffset0 := ssz.ReadOffset(buf[0:4]) // c.SlaveInfoList
	if sszVarOffset0 != 4 {
		return ssz.ErrInvalidVariableOffset
	}
	if sszVarOffset0 > size {
		return ssz.ErrOffset
	}
	sszSlice0 := buf[sszVarOffset0:] // c.SlaveInfoList

	// Field 0: SlaveInfoList
	{
		// empty lists are zero length, so make sure there is room for an offset
		// before attempting to unmarshal it
		if len(sszSlice0) > 3 {
			startOffset := ssz.ReadOffset(sszSlice0[0:4])
			if startOffset == 0 {
				return fmt.Errorf("encountered invalid offset of 0 when decoding c.SlaveInfoList")
			}
			if startOffset%4 != 0 {
				return fmt.Errorf("misaligned list bytes: when decoding c.SlaveInfoList, end-of-list offset is %d, which is not a multiple of 4 (offset size)", startOffset)
			}
			listLen := startOffset / 4
			if listLen > 4294967295 {
				return fmt.Errorf("ssz-max exceeded: c.SlaveInfoList has %d elements, ssz-max is 4294967295: %w", listLen, ssz.ErrListTooBig)
			}
			totalVarBytes := uint64(len(sszSlice0))
			if totalVarBytes < startOffset {
				return fmt.Errorf("list bytes too short to contain an offset when decoding c.SlaveInfoList")
			}
			c.SlaveInfoList = make([]*SlaveInfo, listLen)
			var tmpSlice []byte
			for i := uint64(0); i < listLen; i++ {
				var tmp *SlaveInfo
				tmp = new(SlaveInfo)
				endOffset := totalVarBytes
				if i+1 != listLen {
					endOffset = ssz.ReadOffset(sszSlice0[(i+1)*4 : (i+2)*4])
					if totalVarBytes < endOffset {
						return fmt.Errorf("offset %d points past the end of buffer when decoding c.SlaveInfoList", endOffset)
					}
				}
				if endOffset < startOffset {
					return fmt.Errorf("offset %d is not greater than start offset %d when decoding c.SlaveInfoList", endOffset, startOffset)
				}
				tmpSlice = sszSlice0[startOffset:endOffset]
				if err = tmp.UnmarshalSSZ(tmpSlice); err != nil {
					return fmt.Errorf("SlaveInfoList: %w", err)
				}
				c.SlaveInfoList[i] = tmp
				startOffset = endOffset
			}
		} else {
			if len(sszSlice0) > 0 {
				return fmt.Errorf("list bytes too short to contain an offset when decoding c.SlaveInfoList")
			}
			c.SlaveInfoList = make([]*SlaveInfo, 0)
		}
	}
	return err
}

func (c *ConnectToSlavesRequest) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *ConnectToSlavesRequest) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: SlaveInfoList
	{
		if len(c.SlaveInfoList) > 4294967295 {
			return ssz.ErrListTooBig
		}
		subIndx := hh.Index()
		for _, o := range c.SlaveInfoList {
			if err := o.HashTreeRootWith(hh); err != nil {
				return fmt.Errorf("SlaveInfoList: %w", err)
			}
		}
		hh.MerkleizeWithMixin(subIndx, uint64(len(c.SlaveInfoList)), 4294967295)
	}
	hh.Merkleize(indx)
	return nil
}

func (c *ConnectToSlavesResponse) SizeSSZ() int {
	size := 4
	for _, o := range c.ResultList {
		size += 4
		size += len(o)
	}
	return size
}

func (c *ConnectToSlavesResponse) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *ConnectToSlavesResponse) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error
	offset := 4

	// Field 0: ResultList
	dst = ssz.WriteOffset(dst, offset)
	for _, o := range c.ResultList {
		offset += 4
		offset += len(o)
	}

	// Field 0: ResultList
	if len(c.ResultList) > 4294967295 {
		return nil, ssz.ErrListTooBig
	}
	{
		offset = 4 * len(c.ResultList)
		for _, o := range c.ResultList {
			dst = ssz.WriteOffset(dst, offset)
			offset += len(o)
		}
	}
	for _, o := range c.ResultList {
		if len(o) > 4294967295 {
			return nil, ssz.ErrListTooBig
		}
		dst = append(dst, o...)
	}
	return dst, err
}

func (c *ConnectToSlavesResponse) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size < 4 {
		return ssz.ErrSize
	}

	sszVarOffset0 := ssz.ReadOffset(buf[0:4]) // c.ResultList
	if sszVarOffset0 != 4 {
		return ssz.ErrInvalidVariableOffset
	}
	if sszVarOffset0 > size {
		return ssz.ErrOffset
	}
	sszSlice0 := buf[sszVarOffset0:] // c.ResultList

	// Field 0: ResultList
	{
		// empty lists are zero length, so make sure there is room for an offset
		// before attempting to unmarshal it
		if len(sszSlice0) > 3 {
			startOffset := ssz.ReadOffset(sszSlice0[0:4])
			if startOffset == 0 {
				return fmt.Errorf("encountered invalid offset of 0 when decoding c.ResultList")
			}
			if startOffset%4 != 0 {
				return fmt.Errorf("misaligned list bytes: when decoding c.ResultList, end-of-list offset is %d, which is not a multiple of 4 (offset size)", startOffset)
			}
			listLen := startOffset / 4
			if listLen > 4294967295 {
				return fmt.Errorf("ssz-max exceeded: c.ResultList has %d elements, ssz-max is 4294967295: %w", listLen, ssz.ErrListTooBig)
			}
			totalVarBytes := uint64(len(sszSlice0))
			if totalVarBytes < startOffset {
				return fmt.Errorf("list bytes too short to contain an offset when decoding c.ResultList")
			}
			c.ResultList = make([][]byte, listLen)
			var tmpSlice []byte
			for i := uint64(0); i < listLen; i++ {
				var tmp []byte

				endOffset := totalVarBytes
				if i+1 != listLen {
					endOffset = ssz.ReadOffset(sszSlice0[(i+1)*4 : (i+2)*4])
					if totalVarBytes < endOffset {
						return fmt.Errorf("offset %d points past the end of buffer when decoding c.ResultList", endOffset)
					}
				}
				if endOffset < startOffset {
					return fmt.Errorf("offset %d is not greater than start offset %d when decoding c.ResultList", endOffset, startOffset)
				}
				tmpSlice = sszSlice0[startOffset:endOffset]
				tmp = append([]byte{}, tmpSlice...)
				c.ResultList[i] = tmp
				startOffset = endOffset
			}
		} else {
			if len(sszSlice0) > 0 {
				return fmt.Errorf("list bytes too short to contain an offset when decoding c.ResultList")
			}
			c.ResultList = make([][]byte, 0)
		}
	}
	return err
}

func (c *ConnectToSlavesResponse) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *ConnectToSlavesResponse) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: ResultList
	{
		if len(c.ResultList) > 4294967295 {
			return ssz.ErrListTooBig
		}
		subIndx := hh.Index()
		for _, o := range c.ResultList {

			{
				if len(o) > 4294967295 {
					return ssz.ErrBytesLength
				}
				subIndx := hh.Index()
				hh.AppendBytes32(o)
				numItems := uint64(len(o))
				hh.MerkleizeWithMixin(subIndx, numItems, (4294967295*1+31)/32)
			}

		}
		hh.MerkleizeWithMixin(subIndx, uint64(len(c.ResultList)), 4294967295)
	}
	hh.Merkleize(indx)
	return nil
}

func (c *HelloCommand) SizeSSZ() int {
	size := 98
	size += len(c.RootBlockHeader)
	return size
}

func (c *HelloCommand) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *HelloCommand) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error
	offset := 98

	// Field 0: Version
	dst = binary.LittleEndian.AppendUint32(dst, c.Version)

	// Field 1: NetworkID
	dst = binary.LittleEndian.AppendUint64(dst, c.NetworkID)

	// Field 2: PeerID
	if len(c.PeerID) != 32 {
		return nil, ssz.ErrBytesLength
	}
	dst = append(dst, c.PeerID[:]...)

	// Field 3: PeerIP
	if len(c.PeerIP) != 16 {
		return nil, ssz.ErrBytesLength
	}
	dst = append(dst, c.PeerIP[:]...)

	// Field 4: PeerPort
	dst = binary.LittleEndian.AppendUint16(dst, c.PeerPort)

	// Field 5: RootBlockHeader
	dst = ssz.WriteOffset(dst, offset)
	offset += len(c.RootBlockHeader)

	// Field 6: GenesisRootBlockHash
	if len(c.GenesisRootBlockHash) != 32 {
		return nil, ssz.ErrBytesLength
	}
	dst = append(dst, c.GenesisRootBlockHash[:]...)

	// Field 5: RootBlockHeader
	if len(c.RootBlockHeader) > 4294967295 {
		return nil, ssz.ErrListTooBig
	}
	dst = append(dst, c.RootBlockHeader...)
	return dst, err
}

func (c *HelloCommand) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size < 98 {
		return ssz.ErrSize
	}

	sszSlice0 := buf[0:4]   // c.Version
	sszSlice1 := buf[4:12]  // c.NetworkID
	sszSlice2 := buf[12:44] // c.PeerID
	sszSlice3 := buf[44:60] // c.PeerIP
	sszSlice4 := buf[60:62] // c.PeerPort
	sszSlice6 := buf[66:98] // c.GenesisRootBlockHash

	sszVarOffset5 := ssz.ReadOffset(buf[62:66]) // c.RootBlockHeader
	if sszVarOffset5 != 98 {
		return ssz.ErrInvalidVariableOffset
	}
	if sszVarOffset5 > size {
		return ssz.ErrOffset
	}
	sszSlice5 := buf[sszVarOffset5:] // c.RootBlockHeader

	// Field 0: Version
	c.Version = binary.LittleEndian.Uint32(sszSlice0)

	// Field 1: NetworkID
	c.NetworkID = binary.LittleEndian.Uint64(sszSlice1)

	// Field 2: PeerID
	copy(c.PeerID[:], sszSlice2)

	// Field 3: PeerIP
	copy(c.PeerIP[:], sszSlice3)

	// Field 4: PeerPort
	c.PeerPort = binary.LittleEndian.Uint16(sszSlice4)

	// Field 5: RootBlockHeader
	c.RootBlockHeader = append([]byte{}, Data(sszSlice5)...)

	// Field 6: GenesisRootBlockHash
	copy(c.GenesisRootBlockHash[:], sszSlice6)
	return err
}

func (c *HelloCommand) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *HelloCommand) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: Version
	hh.PutUint32(c.Version)
	// Field 1: NetworkID
	hh.PutUint64(c.NetworkID)
	// Field 2: PeerID
	if len(c.PeerID) != 32 {
		return ssz.ErrBytesLength
	}
	hh.PutBytes(c.PeerID[:])
	// Field 3: PeerIP
	if len(c.PeerIP) != 16 {
		return ssz.ErrBytesLength
	}
	hh.PutBytes(c.PeerIP[:])
	// Field 4: PeerPort
	hh.PutUint16(c.PeerPort)
	// Field 5: RootBlockHeader

	{
		if len(c.RootBlockHeader) > 4294967295 {
			return ssz.ErrBytesLength
		}
		subIndx := hh.Index()
		hh.AppendBytes32(c.RootBlockHeader)
		numItems := uint64(len(c.RootBlockHeader))
		hh.MerkleizeWithMixin(subIndx, numItems, (4294967295*1+31)/32)
	}

	// Field 6: GenesisRootBlockHash
	if len(c.GenesisRootBlockHash) != 32 {
		return ssz.ErrBytesLength
	}
	hh.PutBytes(c.GenesisRootBlockHash[:])
	hh.Merkleize(indx)
	return nil
}

func (c *AddShardBlockHeaderRequest) SizeSSZ() int {
	size := 36
	size += len(c.ShardBlockHeader)
	return size
}

func (c *AddShardBlockHeaderRequest) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *AddShardBlockHeaderRequest) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error
	offset := 36

	// Field 0: ShardBlockHeader
	dst = ssz.WriteOffset(dst, offset)
	offset += len(c.ShardBlockHeader)

	// Field 1: CoinbaseAmount
	dst = binary.LittleEndian.AppendUint64(dst, c.CoinbaseAmount[0])
	dst = binary.LittleEndian.AppendUint64(dst, c.CoinbaseAmount[1])
	dst = binary.LittleEndian.AppendUint64(dst, c.CoinbaseAmount[2])
	dst = binary.LittleEndian.AppendUint64(dst, c.CoinbaseAmount[3])

	// Field 0: ShardBlockHeader
	if len(c.ShardBlockHeader) > 4294967295 {
		return nil, ssz.ErrListTooBig
	}
	dst = append(dst, c.ShardBlockHeader...)
	return dst, err
}

func (c *AddShardBlockHeaderRequest) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size < 36 {
		return ssz.ErrSize
	}

	sszSlice1 := buf[4:36] // c.CoinbaseAmount

	sszVarOffset0 := ssz.ReadOffset(buf[0:4]) // c.ShardBlockHeader
	if sszVarOffset0 != 36 {
		return ssz.ErrInvalidVariableOffset
	}
	if sszVarOffset0 > size {
		return ssz.ErrOffset
	}
	sszSlice0 := buf[sszVarOffset0:] // c.ShardBlockHeader

	// Field 0: ShardBlockHeader
	c.ShardBlockHeader = append([]byte{}, Data(sszSlice0)...)

	// Field 1: CoinbaseAmount
	c.CoinbaseAmount[0] = binary.LittleEndian.Uint64(sszSlice1[0:8])
	c.CoinbaseAmount[1] = binary.LittleEndian.Uint64(sszSlice1[8:16])
	c.CoinbaseAmount[2] = binary.LittleEndian.Uint64(sszSlice1[16:24])
	c.CoinbaseAmount[3] = binary.LittleEndian.Uint64(sszSlice1[24:32])
	return err
}

func (c *AddShardBlockHeaderRequest) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *AddShardBlockHeaderRequest) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: ShardBlockHeader

	{
		if len(c.ShardBlockHeader) > 4294967295 {
			return ssz.ErrBytesLength
		}
		subIndx := hh.Index()
		hh.AppendBytes32(c.ShardBlockHeader)
		numItems := uint64(len(c.ShardBlockHeader))
		hh.MerkleizeWithMixin(subIndx, numItems, (4294967295*1+31)/32)
	}

	// Field 1: CoinbaseAmount
	hh.AppendUint64(c.CoinbaseAmount[0])
	hh.AppendUint64(c.CoinbaseAmount[1])
	hh.AppendUint64(c.CoinbaseAmount[2])
	hh.AppendUint64(c.CoinbaseAmount[3])
	hh.Merkleize(indx)
	return nil
}

func (c *AddShardBlockHeaderResponse) SizeSSZ() int {
	size := 4

	return size
}

func (c *AddShardBlockHeaderResponse) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *AddShardBlockHeaderResponse) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error

	// Field 0: ErrorCode
	dst = binary.LittleEndian.AppendUint32(dst, c.ErrorCode)

	return dst, err
}

func (c *AddShardBlockHeaderResponse) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size != 4 {
		return ssz.ErrSize
	}

	sszSlice0 := buf[0:4] // c.ErrorCode

	// Field 0: ErrorCode
	c.ErrorCode = binary.LittleEndian.Uint32(sszSlice0)
	return err
}

func (c *AddShardBlockHeaderResponse) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *AddShardBlockHeaderResponse) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: ErrorCode
	hh.PutUint32(c.ErrorCode)
	hh.Merkleize(indx)
	return nil
}

func (c *AddXshardTxListRequest) SizeSSZ() int {
	size := 68
	for _, o := range c.DepositTxList {
		size += 4
		size += len(o)
	}
	return size
}

func (c *AddXshardTxListRequest) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *AddXshardTxListRequest) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error
	offset := 68

	// Field 0: ChainID
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[0])
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[1])
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[2])
	dst = binary.LittleEndian.AppendUint64(dst, c.ChainID[3])

	// Field 1: ShardBlockHash
	if len(c.ShardBlockHash) != 32 {
		return nil, ssz.ErrBytesLength
	}
	dst = append(dst, c.ShardBlockHash[:]...)

	// Field 2: DepositTxList
	dst = ssz.WriteOffset(dst, offset)
	for _, o := range c.DepositTxList {
		offset += 4
		offset += len(o)
	}

	// Field 2: DepositTxList
	if len(c.DepositTxList) > 4294967295 {
		return nil, ssz.ErrListTooBig
	}
	{
		offset = 4 * len(c.DepositTxList)
		for _, o := range c.DepositTxList {
			dst = ssz.WriteOffset(dst, offset)
			offset += len(o)
		}
	}
	for _, o := range c.DepositTxList {
		if len(o) > 4294967295 {
			return nil, ssz.ErrListTooBig
		}
		dst = append(dst, o...)
	}
	return dst, err
}

func (c *AddXshardTxListRequest) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size < 68 {
		return ssz.ErrSize
	}

	sszSlice0 := buf[0:32]  // c.ChainID
	sszSlice1 := buf[32:64] // c.ShardBlockHash

	sszVarOffset2 := ssz.ReadOffset(buf[64:68]) // c.DepositTxList
	if sszVarOffset2 != 68 {
		return ssz.ErrInvalidVariableOffset
	}
	if sszVarOffset2 > size {
		return ssz.ErrOffset
	}
	sszSlice2 := buf[sszVarOffset2:] // c.DepositTxList

	// Field 0: ChainID
	c.ChainID[0] = binary.LittleEndian.Uint64(sszSlice0[0:8])
	c.ChainID[1] = binary.LittleEndian.Uint64(sszSlice0[8:16])
	c.ChainID[2] = binary.LittleEndian.Uint64(sszSlice0[16:24])
	c.ChainID[3] = binary.LittleEndian.Uint64(sszSlice0[24:32])

	// Field 1: ShardBlockHash
	copy(c.ShardBlockHash[:], sszSlice1)

	// Field 2: DepositTxList
	{
		// empty lists are zero length, so make sure there is room for an offset
		// before attempting to unmarshal it
		if len(sszSlice2) > 3 {
			startOffset := ssz.ReadOffset(sszSlice2[0:4])
			if startOffset == 0 {
				return fmt.Errorf("encountered invalid offset of 0 when decoding c.DepositTxList")
			}
			if startOffset%4 != 0 {
				return fmt.Errorf("misaligned list bytes: when decoding c.DepositTxList, end-of-list offset is %d, which is not a multiple of 4 (offset size)", startOffset)
			}
			listLen := startOffset / 4
			if listLen > 4294967295 {
				return fmt.Errorf("ssz-max exceeded: c.DepositTxList has %d elements, ssz-max is 4294967295: %w", listLen, ssz.ErrListTooBig)
			}
			totalVarBytes := uint64(len(sszSlice2))
			if totalVarBytes < startOffset {
				return fmt.Errorf("list bytes too short to contain an offset when decoding c.DepositTxList")
			}
			c.DepositTxList = make([]Data, listLen)
			var tmpSlice []byte
			for i := uint64(0); i < listLen; i++ {
				var tmp Data

				endOffset := totalVarBytes
				if i+1 != listLen {
					endOffset = ssz.ReadOffset(sszSlice2[(i+1)*4 : (i+2)*4])
					if totalVarBytes < endOffset {
						return fmt.Errorf("offset %d points past the end of buffer when decoding c.DepositTxList", endOffset)
					}
				}
				if endOffset < startOffset {
					return fmt.Errorf("offset %d is not greater than start offset %d when decoding c.DepositTxList", endOffset, startOffset)
				}
				tmpSlice = sszSlice2[startOffset:endOffset]
				tmp = append([]byte{}, Data(tmpSlice)...)
				c.DepositTxList[i] = tmp
				startOffset = endOffset
			}
		} else {
			if len(sszSlice2) > 0 {
				return fmt.Errorf("list bytes too short to contain an offset when decoding c.DepositTxList")
			}
			c.DepositTxList = make([]Data, 0)
		}
	}
	return err
}

func (c *AddXshardTxListRequest) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *AddXshardTxListRequest) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: ChainID
	hh.AppendUint64(c.ChainID[0])
	hh.AppendUint64(c.ChainID[1])
	hh.AppendUint64(c.ChainID[2])
	hh.AppendUint64(c.ChainID[3])
	// Field 1: ShardBlockHash
	if len(c.ShardBlockHash) != 32 {
		return ssz.ErrBytesLength
	}
	hh.PutBytes(c.ShardBlockHash[:])
	// Field 2: DepositTxList
	{
		if len(c.DepositTxList) > 4294967295 {
			return ssz.ErrListTooBig
		}
		subIndx := hh.Index()
		for _, o := range c.DepositTxList {

			{
				if len(o) > 4294967295 {
					return ssz.ErrBytesLength
				}
				subIndx := hh.Index()
				hh.AppendBytes32(o)
				numItems := uint64(len(o))
				hh.MerkleizeWithMixin(subIndx, numItems, (4294967295*1+31)/32)
			}

		}
		hh.MerkleizeWithMixin(subIndx, uint64(len(c.DepositTxList)), 4294967295)
	}
	hh.Merkleize(indx)
	return nil
}

func (c *AddXshardTxListResponse) SizeSSZ() int {
	size := 4

	return size
}

func (c *AddXshardTxListResponse) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *AddXshardTxListResponse) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error

	// Field 0: ErrorCode
	dst = binary.LittleEndian.AppendUint32(dst, c.ErrorCode)

	return dst, err
}

func (c *AddXshardTxListResponse) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size != 4 {
		return ssz.ErrSize
	}

	sszSlice0 := buf[0:4] // c.ErrorCode

	// Field 0: ErrorCode
	c.ErrorCode = binary.LittleEndian.Uint32(sszSlice0)
	return err
}

func (c *AddXshardTxListResponse) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *AddXshardTxListResponse) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: ErrorCode
	hh.PutUint32(c.ErrorCode)
	hh.Merkleize(indx)
	return nil
}

func (c *AddRootBlockRequest) SizeSSZ() int {
	size := 4
	size += len(c.RootBlock)
	return size
}

func (c *AddRootBlockRequest) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *AddRootBlockRequest) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error
	offset := 4

	// Field 0: RootBlock
	dst = ssz.WriteOffset(dst, offset)
	offset += len(c.RootBlock)

	// Field 0: RootBlock
	if len(c.RootBlock) > 4294967295 {
		return nil, ssz.ErrListTooBig
	}
	dst = append(dst, c.RootBlock...)
	return dst, err
}

func (c *AddRootBlockRequest) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size < 4 {
		return ssz.ErrSize
	}

	sszVarOffset0 := ssz.ReadOffset(buf[0:4]) // c.RootBlock
	if sszVarOffset0 != 4 {
		return ssz.ErrInvalidVariableOffset
	}
	if sszVarOffset0 > size {
		return ssz.ErrOffset
	}
	sszSlice0 := buf[sszVarOffset0:] // c.RootBlock

	// Field 0: RootBlock
	c.RootBlock = append([]byte{}, Data(sszSlice0)...)
	return err
}

func (c *AddRootBlockRequest) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *AddRootBlockRequest) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: RootBlock

	{
		if len(c.RootBlock) > 4294967295 {
			return ssz.ErrBytesLength
		}
		subIndx := hh.Index()
		hh.AppendBytes32(c.RootBlock)
		numItems := uint64(len(c.RootBlock))
		hh.MerkleizeWithMixin(subIndx, numItems, (4294967295*1+31)/32)
	}

	hh.Merkleize(indx)
	return nil
}

func (c *AddRootBlockResponse) SizeSSZ() int {
	size := 4

	return size
}

func (c *AddRootBlockResponse) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *AddRootBlockResponse) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error

	// Field 0: ErrorCode
	dst = binary.LittleEndian.AppendUint32(dst, c.ErrorCode)

	return dst, err
}

func (c *AddRootBlockResponse) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size != 4 {
		return ssz.ErrSize
	}

	sszSlice0 := buf[0:4] // c.ErrorCode

	// Field 0: ErrorCode
	c.ErrorCode = binary.LittleEndian.Uint32(sszSlice0)
	return err
}

func (c *AddRootBlockResponse) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *AddRootBlockResponse) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: ErrorCode
	hh.PutUint32(c.ErrorCode)
	hh.Merkleize(indx)
	return nil
}

func (c *GetUnconfirmedHeadersRequest) SizeSSZ() int {
	size := 0

	return size
}

func (c *GetUnconfirmedHeadersRequest) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *GetUnconfirmedHeadersRequest) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error

	return dst, err
}

func (c *GetUnconfirmedHeadersRequest) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size != 0 {
		return ssz.ErrSize
	}

	return err
}

func (c *GetUnconfirmedHeadersRequest) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *GetUnconfirmedHeadersRequest) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()

	hh.Merkleize(indx)
	return nil
}

func (c *GetUnconfirmedHeadersResponse) SizeSSZ() int {
	size := 8
	for _, o := range c.HeaderList {
		size += 4
		size += len(o)
	}
	return size
}

func (c *GetUnconfirmedHeadersResponse) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *GetUnconfirmedHeadersResponse) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error
	offset := 8

	// Field 0: ErrorCode
	dst = binary.LittleEndian.AppendUint32(dst, c.ErrorCode)

	// Field 1: HeaderList
	dst = ssz.WriteOffset(dst, offset)
	for _, o := range c.HeaderList {
		offset += 4
		offset += len(o)
	}

	// Field 1: HeaderList
	if len(c.HeaderList) > 4294967295 {
		return nil, ssz.ErrListTooBig
	}
	{
		offset = 4 * len(c.HeaderList)
		for _, o := range c.HeaderList {
			dst = ssz.WriteOffset(dst, offset)
			offset += len(o)
		}
	}
	for _, o := range c.HeaderList {
		if len(o) > 4294967295 {
			return nil, ssz.ErrListTooBig
		}
		dst = append(dst, o...)
	}
	return dst, err
}

func (c *GetUnconfirmedHeadersResponse) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size < 8 {
		return ssz.ErrSize
	}

	sszSlice0 := buf[0:4] // c.ErrorCode

	sszVarOffset1 := ssz.ReadOffset(buf[4:8]) // c.HeaderList
	if sszVarOffset1 != 8 {
		return ssz.ErrInvalidVariableOffset
	}
	if sszVarOffset1 > size {
		return ssz.ErrOffset
	}
	sszSlice1 := buf[sszVarOffset1:] // c.HeaderList

	// Field 0: ErrorCode
	c.ErrorCode = binary.LittleEndian.Uint32(sszSlice0)

	// Field 1: HeaderList
	{
		// empty lists are zero length, so make sure there is room for an offset
		// before attempting to unmarshal it
		if len(sszSlice1) > 3 {
			startOffset := ssz.ReadOffset(sszSlice1[0:4])
			if startOffset == 0 {
				return fmt.Errorf("encountered invalid offset of 0 when decoding c.HeaderList")
			}
			if startOffset%4 != 0 {
				return fmt.Errorf("misaligned list bytes: when decoding c.HeaderList, end-of-list offset is %d, which is not a multiple of 4 (offset size)", startOffset)
			}
			listLen := startOffset / 4
			if listLen > 4294967295 {
				return fmt.Errorf("ssz-max exceeded: c.HeaderList has %d elements, ssz-max is 4294967295: %w", listLen, ssz.ErrListTooBig)
			}
			totalVarBytes := uint64(len(sszSlice1))
			if totalVarBytes < startOffset {
				return fmt.Errorf("list bytes too short to contain an offset when decoding c.HeaderList")
			}
			c.HeaderList = make([]Data, listLen)
			var tmpSlice []byte
			for i := uint64(0); i < listLen; i++ {
				var tmp Data

				endOffset := totalVarBytes
				if i+1 != listLen {
					endOffset = ssz.ReadOffset(sszSlice1[(i+1)*4 : (i+2)*4])
					if totalVarBytes < endOffset {
						return fmt.Errorf("offset %d points past the end of buffer when decoding c.HeaderList", endOffset)
					}
				}
				if endOffset < startOffset {
					return fmt.Errorf("offset %d is not greater than start offset %d when decoding c.HeaderList", endOffset, startOffset)
				}
				tmpSlice = sszSlice1[startOffset:endOffset]
				tmp = append([]byte{}, Data(tmpSlice)...)
				c.HeaderList[i] = tmp
				startOffset = endOffset
			}
		} else {
			if len(sszSlice1) > 0 {
				return fmt.Errorf("list bytes too short to contain an offset when decoding c.HeaderList")
			}
			c.HeaderList = make([]Data, 0)
		}
	}
	return err
}

func (c *GetUnconfirmedHeadersResponse) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *GetUnconfirmedHeadersResponse) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: ErrorCode
	hh.PutUint32(c.ErrorCode)
	// Field 1: HeaderList
	{
		if len(c.HeaderList) > 4294967295 {
			return ssz.ErrListTooBig
		}
		subIndx := hh.Index()
		for _, o := range c.HeaderList {

			{
				if len(o) > 4294967295 {
					return ssz.ErrBytesLength
				}
				subIndx := hh.Index()
				hh.AppendBytes32(o)
				numItems := uint64(len(o))
				hh.MerkleizeWithMixin(subIndx, numItems, (4294967295*1+31)/32)
			}

		}
		hh.MerkleizeWithMixin(subIndx, uint64(len(c.HeaderList)), 4294967295)
	}
	hh.Merkleize(indx)
	return nil
}

func (c *GetRootChainStakesRequest) SizeSSZ() int {
	size := 52

	return size
}

func (c *GetRootChainStakesRequest) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *GetRootChainStakesRequest) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error

	// Field 0: Address
	if len(c.Address) != 20 {
		return nil, ssz.ErrBytesLength
	}
	dst = append(dst, c.Address[:]...)

	// Field 1: ShardBlockHash
	if len(c.ShardBlockHash) != 32 {
		return nil, ssz.ErrBytesLength
	}
	dst = append(dst, c.ShardBlockHash[:]...)

	return dst, err
}

func (c *GetRootChainStakesRequest) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size != 52 {
		return ssz.ErrSize
	}

	sszSlice0 := buf[0:20]  // c.Address
	sszSlice1 := buf[20:52] // c.ShardBlockHash

	// Field 0: Address
	copy(c.Address[:], sszSlice0)

	// Field 1: ShardBlockHash
	copy(c.ShardBlockHash[:], sszSlice1)
	return err
}

func (c *GetRootChainStakesRequest) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *GetRootChainStakesRequest) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: Address
	if len(c.Address) != 20 {
		return ssz.ErrBytesLength
	}
	hh.PutBytes(c.Address[:])
	// Field 1: ShardBlockHash
	if len(c.ShardBlockHash) != 32 {
		return ssz.ErrBytesLength
	}
	hh.PutBytes(c.ShardBlockHash[:])
	hh.Merkleize(indx)
	return nil
}

func (c *GetRootChainStakesResponse) SizeSSZ() int {
	size := 56

	return size
}

func (c *GetRootChainStakesResponse) MarshalSSZ() ([]byte, error) {
	buf := make([]byte, c.SizeSSZ())
	return c.MarshalSSZTo(buf[:0])
}

func (c *GetRootChainStakesResponse) MarshalSSZTo(dst []byte) ([]byte, error) {
	var err error

	// Field 0: ErrorCode
	dst = binary.LittleEndian.AppendUint32(dst, c.ErrorCode)

	// Field 1: Stakes
	dst = binary.LittleEndian.AppendUint64(dst, c.Stakes[0])
	dst = binary.LittleEndian.AppendUint64(dst, c.Stakes[1])
	dst = binary.LittleEndian.AppendUint64(dst, c.Stakes[2])
	dst = binary.LittleEndian.AppendUint64(dst, c.Stakes[3])

	// Field 2: Signer
	if len(c.Signer) != 20 {
		return nil, ssz.ErrBytesLength
	}
	dst = append(dst, c.Signer[:]...)

	return dst, err
}

func (c *GetRootChainStakesResponse) UnmarshalSSZ(buf []byte) error {
	var err error
	size := uint64(len(buf))
	if size != 56 {
		return ssz.ErrSize
	}

	sszSlice0 := buf[0:4]   // c.ErrorCode
	sszSlice1 := buf[4:36]  // c.Stakes
	sszSlice2 := buf[36:56] // c.Signer

	// Field 0: ErrorCode
	c.ErrorCode = binary.LittleEndian.Uint32(sszSlice0)

	// Field 1: Stakes
	c.Stakes[0] = binary.LittleEndian.Uint64(sszSlice1[0:8])
	c.Stakes[1] = binary.LittleEndian.Uint64(sszSlice1[8:16])
	c.Stakes[2] = binary.LittleEndian.Uint64(sszSlice1[16:24])
	c.Stakes[3] = binary.LittleEndian.Uint64(sszSlice1[24:32])

	// Field 2: Signer
	copy(c.Signer[:], sszSlice2)
	return err
}

func (c *GetRootChainStakesResponse) HashTreeRoot() ([32]byte, error) {
	hh := ssz.DefaultHasherPool.Get()
	if err := c.HashTreeRootWith(hh); err != nil {
		ssz.DefaultHasherPool.Put(hh)
		return [32]byte{}, err
	}
	root, err := hh.HashRoot()
	ssz.DefaultHasherPool.Put(hh)
	return root, err
}

func (c *GetRootChainStakesResponse) HashTreeRootWith(hh *ssz.Hasher) (err error) {
	indx := hh.Index()
	// Field 0: ErrorCode
	hh.PutUint32(c.ErrorCode)
	// Field 1: Stakes
	hh.AppendUint64(c.Stakes[0])
	hh.AppendUint64(c.Stakes[1])
	hh.AppendUint64(c.Stakes[2])
	hh.AppendUint64(c.Stakes[3])
	// Field 2: Signer
	if len(c.Signer) != 20 {
		return ssz.ErrBytesLength
	}
	hh.PutBytes(c.Signer[:])
	hh.Merkleize(indx)
	return nil
}
