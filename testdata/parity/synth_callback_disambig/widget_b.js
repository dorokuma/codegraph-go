import { Observable } from "./observable.js";

export class WidgetB {
  constructor() {
    this.bus = new Observable();
    this.bus.onUpdate(this.handle);
  }
  handle() {
    this.doB();
  }
  doB() {}
  fire() {
    this.bus.triggerUpdate();
  }
}
