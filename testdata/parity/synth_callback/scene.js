// Field-backed observer: registrar + dispatcher share this.callbacks.
export class Scene {
  constructor() {
    this.callbacks = new Set();
  }
  onUpdate(cb) {
    this.callbacks.add(cb);
  }
  triggerUpdate() {
    for (const cb of this.callbacks) {
      cb();
    }
  }
}
