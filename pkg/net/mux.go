package net

import (
    "fmt"
    "io"
    "sync"
)

// MuxConn 多路复用连接
type MuxConn struct {
    conn    io.ReadWriteCloser
    reader  *io.PipeReader
    writer  *io.PipeWriter
    mu      sync.RWMutex
}

// NewMuxConn 创建多路复用连接
func NewMuxConn(conn io.ReadWriteCloser) *MuxConn {
    reader, writer := io.Pipe()
    
    return &MuxConn{
        conn:   conn,
        reader: reader,
        writer: writer,
    }
}

// Read 读取数据
func (m *MuxConn) Read(p []byte) (n int, err error) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    return m.reader.Read(p)
}

// Write 写入数据
func (m *MuxConn) Write(p []byte) (n int, err error) {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    return m.writer.Write(p)
}

// Close 关闭连接
func (m *MuxConn) Close() error {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    err := m.writer.Close()
    if err != nil {
        return err
    }
    
    return m.reader.Close()
}

// LocalAddr 获取本地地址
func (m *MuxConn) LocalAddr() string {
    return m.conn.LocalAddr().String()
}

// RemoteAddr 获取远程地址
func (m *MuxConn) RemoteAddr() string {
    return m.conn.RemoteAddr().String()
}
