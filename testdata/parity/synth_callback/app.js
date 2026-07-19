import { Scene } from "./scene.js";

export class App {
  constructor() {
    this.scene = new Scene();
    // Registration site: wires triggerRender into the observer channel.
    this.scene.onUpdate(this.triggerRender);
  }
  mutateElement() {
    this.doWork();
    this.scene.triggerUpdate();
  }
  doWork() {}
  triggerRender() {
    paintCanvas();
  }
}

export function paintCanvas() {}
