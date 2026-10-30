import net from 'node:net';

/**
 * A TCP proxy in front of a Bazel, to drop connections the way a laptop going
 * to sleep or a network blip does: the server stays up and the page's
 * EventSource dies. Browsers keep their connections in a pool, so drop() cuts
 * them all and refuses new ones until resume().
 */
export class FlakyProxy {
  private server?: net.Server;
  private socks = new Set<net.Socket>();
  private down = false;
  port = 0;
  constructor(private readonly target: number) {}

  async start(): Promise<void> {
    this.server = net.createServer((c) => {
      if (this.down) return c.destroy();
      const s = net.connect(this.target, '127.0.0.1');
      for (const x of [c, s]) {
        this.socks.add(x);
        x.on('error', () => {});
        x.on('close', () => this.socks.delete(x));
      }
      c.pipe(s).pipe(c);
    });
    await new Promise<void>((r) => this.server!.listen(0, '127.0.0.1', () => r()));
    this.port = (this.server.address() as net.AddressInfo).port;
  }

  drop(): void {
    this.down = true;
    for (const s of this.socks) s.destroy();
  }

  resume(): void {
    this.down = false;
  }

  stop(): void {
    this.drop();
    this.server?.close();
  }
}
