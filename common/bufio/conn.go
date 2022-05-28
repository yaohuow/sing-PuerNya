package bufio

import (
	"context"
	"io"
	"net"

	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/rw"
	"github.com/sagernet/sing/common/task"
)

func Copy(dst io.Writer, src io.Reader) (n int64, err error) {
	src = N.UnwrapReader(src)
	dst = N.UnwrapWriter(dst)
	if wt, ok := src.(io.WriterTo); ok {
		return wt.WriteTo(dst)
	}
	if rt, ok := dst.(io.ReaderFrom); ok {
		return rt.ReadFrom(src)
	}
	extendedSrc, srcExtended := src.(N.ExtendedReader)
	extendedDst, dstExtended := dst.(N.ExtendedWriter)
	if !srcExtended {
		extendedSrc = &ExtendedReaderWrapper{src}
	}
	if !dstExtended {
		extendedDst = &ExtendedWriterWrapper{dst}
	}
	return CopyExtended(extendedDst, extendedSrc)
}

func CopyExtended(dst N.ExtendedWriter, src N.ExtendedReader) (n int64, err error) {
	if _, unsafe := common.Cast[N.ThreadUnsafeWriter](dst); unsafe {
		return CopyExtendedWithPool(dst, src)
	}

	_buffer := buf.StackNew()
	defer common.KeepAlive(_buffer)
	buffer := common.Dup(_buffer)
	buffer.IncRef()
	defer buffer.DecRef()
	for {
		buffer.Reset()
		err = src.ReadBuffer(buffer)
		if err != nil {
			return
		}
		dataLen := buffer.Len()
		err = dst.WriteBuffer(buffer)
		if err != nil {
			return
		}
		n += int64(dataLen)
	}
}

func CopyExtendedWithPool(dst N.ExtendedWriter, src N.ExtendedReader) (n int64, err error) {
	for {
		buffer := buf.New()
		err = src.ReadBuffer(buffer)
		if err != nil {
			buffer.Release()
			return
		}
		dataLen := buffer.Len()
		err = dst.WriteBuffer(buffer)
		if err != nil {
			buffer.Release()
			return
		}
		n += int64(dataLen)
	}
}

func CopyConn(ctx context.Context, conn net.Conn, dest net.Conn) error {
	defer common.Close(conn, dest)
	err := task.Run(ctx, func() error {
		defer rw.CloseRead(conn)
		defer rw.CloseWrite(dest)
		return common.Error(Copy(dest, conn))
	}, func() error {
		defer rw.CloseRead(dest)
		defer rw.CloseWrite(conn)
		return common.Error(Copy(conn, dest))
	})
	return err
}

func CopyPacket(dst N.PacketWriter, src N.PacketReader) (n int64, err error) {
	if _, unsafe := common.Cast[N.ThreadUnsafeWriter](dst); unsafe {
		return CopyPacketWithPool(dst, src)
	}

	_buffer := buf.StackNewPacket()
	defer common.KeepAlive(_buffer)
	buffer := common.Dup(_buffer)
	buffer.IncRef()
	defer buffer.DecRef()
	var destination M.Socksaddr
	for {
		buffer.Reset()
		destination, err = src.ReadPacket(buffer)
		if err != nil {
			return
		}
		dataLen := buffer.Len()
		err = dst.WritePacket(buffer, destination)
		if err != nil {
			return
		}
		n += int64(dataLen)
	}
}

func CopyPacketWithPool(dest N.PacketWriter, src N.PacketReader) (n int64, err error) {
	var destination M.Socksaddr
	for {
		buffer := buf.New()
		destination, err = src.ReadPacket(buffer)
		if err != nil {
			buffer.Release()
			return
		}
		dataLen := buffer.Len()
		err = dest.WritePacket(buffer, destination)
		if err != nil {
			buffer.Release()
			return
		}
		n += int64(dataLen)
	}
}

func CopyPacketConn(ctx context.Context, conn N.PacketConn, dest N.PacketConn) error {
	defer common.Close(conn, dest)
	return task.Any(ctx, func() error {
		return common.Error(CopyPacket(dest, conn))
	}, func() error {
		return common.Error(CopyPacket(conn, dest))
	})
}

func CopyNetPacketConn(ctx context.Context, conn N.PacketConn, dest net.PacketConn) error {
	if udpConn, ok := dest.(*net.UDPConn); ok {
		return CopyPacketConn(ctx, conn, &UDPConnWrapper{udpConn})
	} else {
		return CopyPacketConn(ctx, conn, &PacketConnWrapper{dest})
	}
}

type UDPConnWrapper struct {
	*net.UDPConn
}

func (w *UDPConnWrapper) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	n, addr, err := w.ReadFromUDPAddrPort(buffer.FreeBytes())
	if err != nil {
		return M.Socksaddr{}, err
	}
	buffer.Truncate(n)
	return M.SocksaddrFromNetIP(addr), nil
}

func (w *UDPConnWrapper) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	defer buffer.Release()
	if destination.Family().IsFqdn() {
		udpAddr, err := net.ResolveUDPAddr("udp", destination.String())
		if err != nil {
			return err
		}
		return common.Error(w.UDPConn.WriteTo(buffer.Bytes(), udpAddr))
	}
	return common.Error(w.UDPConn.WriteToUDP(buffer.Bytes(), destination.UDPAddr()))
}

type PacketConnWrapper struct {
	net.PacketConn
}

func (p *PacketConnWrapper) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	_, addr, err := buffer.ReadPacketFrom(p)
	if err != nil {
		return M.Socksaddr{}, err
	}
	return M.SocksaddrFromNet(addr), err
}

func (p *PacketConnWrapper) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	defer buffer.Release()
	return common.Error(p.WriteTo(buffer.Bytes(), destination.UDPAddr()))
}

type BindPacketConn struct {
	net.PacketConn
	Addr net.Addr
}

func (c *BindPacketConn) Read(b []byte) (n int, err error) {
	n, _, err = c.ReadFrom(b)
	return
}

func (c *BindPacketConn) Write(b []byte) (n int, err error) {
	return c.WriteTo(b, c.Addr)
}

func (c *BindPacketConn) RemoteAddr() net.Addr {
	return c.Addr
}

type ExtendedReaderWrapper struct {
	io.Reader
}

func (r *ExtendedReaderWrapper) ReadBuffer(buffer *buf.Buffer) error {
	n, err := r.Read(buffer.FreeBytes())
	if err != nil {
		return err
	}
	buffer.Truncate(n)
	return nil
}

func (r *ExtendedReaderWrapper) Upstream() any {
	return r.Reader
}

type ExtendedWriterWrapper struct {
	io.Writer
}

func (w *ExtendedWriterWrapper) WriteBuffer(buffer *buf.Buffer) error {
	return common.Error(w.Write(buffer.Bytes()))
}

func (r *ExtendedWriterWrapper) Upstream() any {
	return r.Writer
}
