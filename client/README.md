# client/

`aethertunnel-client` 的命令入口：旗标解析、配置装载、信号处理，然后调用
[`pkg/clientlib`](../pkg/clientlib) 的 `Run`。实现都在库里，这里只剩壳。

The command entry of `aethertunnel-client`: flag parsing, configuration loading and
signal handling, then one call into [`pkg/clientlib`](../pkg/clientlib).
