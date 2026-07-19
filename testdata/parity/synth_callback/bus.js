// EventEmitter-style string-keyed channel.
export function publishMount(bus) {
  bus.emit("mount", this);
}

export function setupBus(bus) {
  bus.on("mount", onmount);
}

export function onmount() {
  afterMount();
}

export function afterMount() {}
