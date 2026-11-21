// The Three.js side of the digital twin. It owns the renderer and scene, and takes all of
// its data from TwinState, so React never re-renders on animation frames.
import * as THREE from "three";
import { OrbitControls } from "three/examples/jsm/controls/OrbitControls.js";
import { COMPONENT_ORDER, litModules, WHEEL_SLOWDOWN, wheelRate, type ComponentId, type Tone, type TwinState } from "./twin";

const TONE_COLORS: Record<Tone, number> = { ok: 0x0ca30c, warning: 0xfab219, critical: 0xd03b3b, inactive: 0x5c5b56 };
const STALE_COLOR = 0x5c5b56;
const MODULE_COLUMNS = 8;
const MODULE_ROWS = 3;
const WHEEL_RADIUS = 0.34;
const CAMERA_HOME = new THREE.Vector3(4.6, 2.5, 5.2);
const TARGET_HOME = new THREE.Vector3(0, 0.6, 0);

/** Time for values to settle, short enough that a new reading is visible at once. */
const COLOR_SETTLE_S = 0.15;
const SPEED_SETTLE_S = 0.5;

interface Part {
  id: ComponentId;
  /** Where the camera looks and the diagnostic marker sits. */
  anchor: THREE.Vector3;
  materials: THREE.MeshStandardMaterial[];
  color: THREE.Color;
  marker: THREE.Sprite;
  markerLevel: string | null;
}

export interface TwinSceneOptions {
  onSelect: (id: ComponentId | null) => void;
  onHover: (id: ComponentId | null) => void;
}

function bodyShape(): THREE.Shape {
  // Side profile with the nose towards +x, including the wheel arches.
  const s = new THREE.Shape();
  s.moveTo(-2.2, 0.32);
  s.lineTo(-1.84, 0.32);
  s.absarc(-1.4, 0.34, 0.44, Math.PI, 0, true);
  s.lineTo(0.96, 0.32);
  s.absarc(1.4, 0.34, 0.44, Math.PI, 0, true);
  s.lineTo(2.2, 0.32);
  s.lineTo(2.28, 0.62);
  s.quadraticCurveTo(2.2, 0.82, 1.8, 0.9);
  s.lineTo(1.0, 1.02);
  s.quadraticCurveTo(0.55, 1.42, 0.15, 1.46);
  s.lineTo(-0.85, 1.46);
  s.quadraticCurveTo(-1.5, 1.42, -1.95, 1.08);
  s.quadraticCurveTo(-2.22, 1.0, -2.26, 0.7);
  s.closePath();
  return s;
}

function markerTexture(level: string): THREE.CanvasTexture {
  // Shape as well as colour, matching the rest of the dashboard.
  const canvas = document.createElement("canvas");
  canvas.width = canvas.height = 128;
  const ctx = canvas.getContext("2d")!;
  ctx.beginPath();
  if (level === "critical") {
    ctx.moveTo(64, 8);
    ctx.lineTo(120, 64);
    ctx.lineTo(64, 120);
    ctx.lineTo(8, 64);
  } else {
    ctx.moveTo(64, 12);
    ctx.lineTo(120, 112);
    ctx.lineTo(8, 112);
  }
  ctx.closePath();
  ctx.fillStyle = level === "critical" ? "#d03b3b" : level === "warning" ? "#fab219" : "#898781";
  ctx.fill();
  ctx.lineWidth = 8;
  ctx.strokeStyle = "#0d0d0d";
  ctx.stroke();
  ctx.fillStyle = level === "warning" ? "#0b0b0b" : "#ffffff";
  ctx.font = "bold 64px system-ui, sans-serif";
  ctx.textAlign = "center";
  ctx.textBaseline = "middle";
  ctx.fillText("!", 64, level === "critical" ? 68 : 78);
  const texture = new THREE.CanvasTexture(canvas);
  texture.colorSpace = THREE.SRGBColorSpace;
  return texture;
}

function roadTexture(): THREE.CanvasTexture {
  const canvas = document.createElement("canvas");
  canvas.width = 256;
  canvas.height = 64;
  const ctx = canvas.getContext("2d")!;
  ctx.fillStyle = "#161615";
  ctx.fillRect(0, 0, 256, 64);
  // Lane lines either side of the vehicle, which show motion when the road scrolls.
  ctx.fillStyle = "#3a3a37";
  ctx.fillRect(0, 7, 128, 1);
  ctx.fillRect(0, 56, 128, 1);
  const texture = new THREE.CanvasTexture(canvas);
  texture.wrapS = THREE.RepeatWrapping;
  texture.colorSpace = THREE.SRGBColorSpace;
  texture.repeat.set(4, 1);
  return texture;
}

export class TwinScene {
  private renderer: THREE.WebGLRenderer;
  private scene = new THREE.Scene();
  private camera = new THREE.PerspectiveCamera(36, 1, 0.1, 100);
  private controls: OrbitControls;
  private lastFrame = performance.now();
  private raycaster = new THREE.Raycaster();
  private parts = new Map<ComponentId, Part>();
  private pickable: THREE.Object3D[] = [];
  private textures = new Map<string, THREE.CanvasTexture>();
  private wheels: THREE.Group[] = [];
  private modules: THREE.Mesh[] = [];
  private road!: THREE.CanvasTexture;
  private chargeCable!: THREE.Mesh;
  private chargePulse!: THREE.Mesh;
  private chargeCurve!: THREE.CatmullRomCurve3;
  private chargePost!: THREE.Group;
  private bodyEdges!: THREE.LineSegments;
  private state: TwinState | null = null;
  private selected: ComponentId | null = null;
  private hovered: ComponentId | null = null;
  private speed = 0;
  private batteryHeat = 0;
  private motorHeat = 0;
  private pulse = 0;
  private cameraGoal: THREE.Vector3 | null = null;
  private targetGoal: THREE.Vector3 | null = null;
  private visible = true;
  private reducedMotion: MediaQueryList;
  private observers: { disconnect(): void }[] = [];
  private pointerDown: { x: number; y: number } | null = null;
  private frames = 0;

  constructor(
    private container: HTMLElement,
    private options: TwinSceneOptions,
  ) {
    this.renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true });
    this.renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
    container.appendChild(this.renderer.domElement);
    this.reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");

    this.camera.position.copy(CAMERA_HOME);
    this.controls = new OrbitControls(this.camera, this.renderer.domElement);
    this.controls.target.copy(TARGET_HOME);
    this.controls.enableDamping = true;
    this.controls.enablePan = false;
    this.controls.minDistance = 3;
    this.controls.maxDistance = 14;
    this.controls.maxPolarAngle = Math.PI * 0.495;
    // User input cancels any camera move in progress.
    this.controls.addEventListener("start", () => {
      this.cameraGoal = null;
      this.targetGoal = null;
    });

    this.buildScene();
    this.bindEvents();
    this.resize();
    this.renderer.setAnimationLoop(() => this.frame());
  }

  /** Applies the latest telemetry. Called for each sample from the live stream. */
  update(state: TwinState): void {
    this.state = state;
    const lit = litModules(state.socPct, this.modules.length);
    this.modules.forEach((m, i) => {
      const material = m.material as THREE.MeshStandardMaterial;
      material.opacity = i < lit ? 1 : 0.22;
    });
    this.chargeCable.visible = state.charging.active;
    this.chargePulse.visible = state.charging.active;
    this.chargePost.visible = state.charging.active;
    (this.bodyEdges.material as THREE.LineBasicMaterial).color.setHex(state.stale ? 0x4a4a46 : 0x8b8a84);
    for (const id of COMPONENT_ORDER) {
      const part = this.parts.get(id)!;
      const c = state.components[id];
      part.color.setHex(state.stale ? STALE_COLOR : TONE_COLORS[c.tone]);
      this.setMarker(part, c.marker);
    }
  }

  select(id: ComponentId | null): void {
    this.selected = id;
    const part = id && this.parts.get(id);
    if (part) {
      const offset = this.camera.position.clone().sub(this.controls.target).setLength(5.2);
      this.targetGoal = part.anchor.clone();
      this.cameraGoal = part.anchor.clone().add(offset);
    }
  }

  hover(id: ComponentId | null): void {
    this.hovered = id;
  }

  resetView(): void {
    this.cameraGoal = CAMERA_HOME.clone();
    this.targetGoal = TARGET_HOME.clone();
  }

  /** Used by tests and diagnostics to check the model is rendering and what it shows. */
  debugState() {
    return {
      frames: this.frames,
      speed: this.speed,
      wheelAngle: this.wheels[0]?.children[0]?.rotation.y ?? 0,
      litModules: this.modules.filter((m) => (m.material as THREE.MeshStandardMaterial).opacity === 1).length,
      charging: this.chargeCable.visible,
      colors: Object.fromEntries(COMPONENT_ORDER.map((id) => [id, `#${this.parts.get(id)!.color.getHexString()}`])),
      markers: Object.fromEntries(COMPONENT_ORDER.map((id) => [id, this.parts.get(id)!.markerLevel])),
    };
  }

  dispose(): void {
    this.renderer.setAnimationLoop(null);
    this.observers.forEach((o) => o.disconnect());
    this.controls.dispose();
    this.scene.traverse((o) => {
      const mesh = o as THREE.Mesh;
      mesh.geometry?.dispose();
      const material = mesh.material as THREE.Material | THREE.Material[] | undefined;
      (Array.isArray(material) ? material : material ? [material] : []).forEach((m) => m.dispose());
    });
    this.textures.forEach((t) => t.dispose());
    this.road.dispose();
    this.renderer.dispose();
    // Browsers cap live WebGL contexts, so repeated page visits must not leave them open.
    this.renderer.forceContextLoss();
    this.renderer.domElement.remove();
  }

  private bindEvents(): void {
    const el = this.renderer.domElement;
    const resize = new ResizeObserver(() => this.resize());
    resize.observe(this.container);
    // The render loop stops while the model is off screen, to spare the viewer's GPU.
    const seen = new IntersectionObserver(([entry]) => {
      this.visible = Boolean(entry?.isIntersecting);
    });
    seen.observe(this.container);
    this.observers.push(resize, seen);

    el.addEventListener("pointerdown", (e) => {
      this.pointerDown = { x: e.clientX, y: e.clientY };
    });
    el.addEventListener("pointerup", (e) => {
      const start = this.pointerDown;
      this.pointerDown = null;
      // A drag orbits the camera. Only a click selects.
      if (start && Math.abs(e.clientX - start.x) + Math.abs(e.clientY - start.y) < 5) {
        this.options.onSelect(this.pick(e));
      }
    });
    el.addEventListener("pointermove", (e) => {
      if (this.pointerDown) return;
      const id = this.pick(e);
      if (id !== this.hovered) {
        this.hovered = id;
        el.style.cursor = id ? "pointer" : "grab";
        this.options.onHover(id);
      }
    });
    el.addEventListener("pointerleave", () => {
      if (this.hovered) {
        this.hovered = null;
        this.options.onHover(null);
      }
    });
  }

  private pick(e: PointerEvent): ComponentId | null {
    const box = this.renderer.domElement.getBoundingClientRect();
    const ndc = new THREE.Vector2(((e.clientX - box.left) / box.width) * 2 - 1, -((e.clientY - box.top) / box.height) * 2 + 1);
    this.raycaster.setFromCamera(ndc, this.camera);
    for (const hit of this.raycaster.intersectObjects(this.pickable, true)) {
      let o: THREE.Object3D | null = hit.object;
      while (o) {
        if (o.visible === false) break;
        const id = o.userData.part as ComponentId | undefined;
        if (id) return id;
        o = o.parent;
      }
    }
    return null;
  }

  private resize(): void {
    const w = this.container.clientWidth;
    const h = this.container.clientHeight;
    if (w === 0 || h === 0) return;
    this.renderer.setSize(w, h);
    this.camera.aspect = w / h;
    // On a narrow screen, widen the view so the whole vehicle stays in frame.
    this.camera.fov = w / h < 1 ? 52 : 36;
    this.camera.updateProjectionMatrix();
  }

  private frame(): void {
    const now = performance.now();
    const elapsed = (now - this.lastFrame) / 1000;
    this.lastFrame = now;
    if (!this.visible || document.hidden) return;
    const dt = Math.min(elapsed, 0.1);
    const moving = !this.reducedMotion.matches;
    this.frames++;
    const target = this.state;
    if (target) {
      this.speed = THREE.MathUtils.damp(this.speed, target.speedKph, 1 / SPEED_SETTLE_S, dt);
      if (target.speedKph === 0 && this.speed < 0.2) this.speed = 0;
      this.batteryHeat = THREE.MathUtils.damp(this.batteryHeat, target.batteryHeat, 1 / COLOR_SETTLE_S, dt);
      this.motorHeat = THREE.MathUtils.damp(this.motorHeat, target.motorHeat, 1 / COLOR_SETTLE_S, dt);
    }
    if (moving) {
      this.pulse += dt;
      const spin = wheelRate(this.speed) * dt;
      for (const wheel of this.wheels) wheel.children[0]!.rotation.y -= spin;
      // Moves with the wheels: the same slowed rate, over 40 units of road per 4 repeats.
      this.road.offset.x += ((this.speed / 3.6) * dt * WHEEL_SLOWDOWN) / 10;
    }
    this.applyParts(dt, moving);
    if (target?.charging.active && this.chargePulse.visible) {
      const rate = 0.25 + Math.min(target.charging.powerKw, 250) / 250;
      const t = moving ? (this.pulse * rate) % 1 : 0.5;
      this.chargePulse.position.copy(this.chargeCurve.getPoint(t));
    }
    if (this.cameraGoal && this.targetGoal) {
      const k = moving ? 1 - Math.exp(-5 * dt) : 1;
      this.camera.position.lerp(this.cameraGoal, k);
      this.controls.target.lerp(this.targetGoal, k);
      if (this.camera.position.distanceTo(this.cameraGoal) < 0.01) {
        this.cameraGoal = null;
        this.targetGoal = null;
      }
    }
    this.controls.update();
    this.renderer.render(this.scene, this.camera);
  }

  private applyParts(dt: number, moving: boolean): void {
    const blend = 1 - Math.exp(-dt / COLOR_SETTLE_S);
    const beat = moving ? 0.5 + 0.5 * Math.sin(this.pulse * 5) : 1;
    for (const part of this.parts.values()) {
      const focus = part.id === this.selected || part.id === this.hovered;
      const heat = part.id === "battery" ? this.batteryHeat : part.id === "motor" ? this.motorHeat : 0;
      const charging = part.id === "charge_port" && this.state?.charging.active ? 0.4 + 0.6 * beat : 0;
      const glow = 0.25 + heat * 0.9 + charging + (focus ? 0.5 : 0);
      for (const m of part.materials) {
        m.color.lerp(part.color, blend);
        m.emissive.lerp(part.color, blend);
        m.emissiveIntensity = glow;
      }
      if (part.marker.visible) {
        part.marker.scale.setScalar(0.28 + (moving ? 0.05 * beat : 0));
      }
    }
  }

  private setMarker(part: Part, level: string | null): void {
    if (part.markerLevel === level) return;
    part.markerLevel = level;
    part.marker.visible = level !== null;
    if (level) {
      let texture = this.textures.get(level);
      if (!texture) {
        texture = markerTexture(level);
        this.textures.set(level, texture);
      }
      part.marker.material.map = texture;
      part.marker.material.needsUpdate = true;
    }
  }

  private material(color = TONE_COLORS.inactive): THREE.MeshStandardMaterial {
    return new THREE.MeshStandardMaterial({ color, emissive: color, emissiveIntensity: 0.25, roughness: 0.55, metalness: 0.15 });
  }

  /** Registers a group of meshes as one selectable part of the vehicle. */
  private addPart(id: ComponentId, group: THREE.Group, anchor: THREE.Vector3, materials: THREE.MeshStandardMaterial[]): void {
    group.userData.part = id;
    this.scene.add(group);
    this.pickable.push(group);
    const marker = new THREE.Sprite(new THREE.SpriteMaterial({ depthTest: false, transparent: true }));
    marker.position.copy(anchor).add(new THREE.Vector3(0, 0.36, 0));
    marker.scale.setScalar(0.28);
    marker.visible = false;
    marker.renderOrder = 10;
    marker.userData.part = id;
    this.scene.add(marker);
    this.pickable.push(marker);
    this.parts.set(id, { id, anchor, materials, color: new THREE.Color(TONE_COLORS.inactive), marker, markerLevel: null });
  }

  private buildScene(): void {
    this.scene.add(new THREE.AmbientLight(0xffffff, 0.8));
    const key = new THREE.DirectionalLight(0xffffff, 1.6);
    key.position.set(5, 8, 4);
    this.scene.add(key);

    this.road = roadTexture();
    const road = new THREE.Mesh(new THREE.PlaneGeometry(40, 8), new THREE.MeshBasicMaterial({ map: this.road }));
    road.rotation.x = -Math.PI / 2;
    this.scene.add(road);
    const grid = new THREE.GridHelper(40, 40, 0x2c2c2a, 0x222220);
    grid.position.y = -0.002;
    this.scene.add(grid);

    this.buildBody();
    this.buildWheels();
    this.buildBattery();
    this.buildMotor();
    this.buildCooling();
    this.buildChargePort();
    this.buildAntenna();
    this.buildSensor();
    this.buildElectronics();
  }

  private buildBody(): void {
    const geometry = new THREE.ExtrudeGeometry(bodyShape(), { depth: 1.74, bevelEnabled: true, bevelSize: 0.05, bevelThickness: 0.06, bevelSegments: 2, curveSegments: 14 });
    geometry.translate(0, 0, -0.87);
    // The shell is see-through so that the parts inside stay visible. It does not write
    // depth, and is not pickable, so clicks reach those parts.
    const shell = new THREE.Mesh(
      geometry,
      new THREE.MeshStandardMaterial({ color: 0x8fa3b5, transparent: true, opacity: 0.1, depthWrite: false, roughness: 0.3, side: THREE.DoubleSide }),
    );
    this.scene.add(shell);
    this.bodyEdges = new THREE.LineSegments(new THREE.EdgesGeometry(geometry, 22), new THREE.LineBasicMaterial({ color: 0x8b8a84, transparent: true, opacity: 0.75 }));
    this.scene.add(this.bodyEdges);
  }

  private buildWheels(): void {
    const tyre = new THREE.CylinderGeometry(WHEEL_RADIUS, WHEEL_RADIUS, 0.26, 28);
    const hub = new THREE.CylinderGeometry(0.2, 0.2, 0.28, 16);
    const spoke = new THREE.BoxGeometry(0.06, 0.29, 0.34);
    const tyreMaterial = new THREE.MeshStandardMaterial({ color: 0x232322, roughness: 0.9 });
    const hubMaterial = new THREE.MeshStandardMaterial({ color: 0x777670, roughness: 0.4, metalness: 0.6 });
    for (const [x, z] of [
      [1.4, 0.82],
      [1.4, -0.82],
      [-1.4, 0.82],
      [-1.4, -0.82],
    ] as const) {
      const wheel = new THREE.Group();
      wheel.position.set(x, WHEEL_RADIUS, z);
      wheel.rotation.x = Math.PI / 2;
      // The inner group spins on the axle. Spokes make that rotation visible.
      const spinner = new THREE.Group();
      spinner.add(new THREE.Mesh(tyre, tyreMaterial), new THREE.Mesh(hub, hubMaterial));
      for (let i = 0; i < 3; i++) {
        const s = new THREE.Mesh(spoke, tyreMaterial);
        s.rotation.y = (i * Math.PI) / 3;
        spinner.add(s);
      }
      wheel.add(spinner);
      this.wheels.push(wheel);
      this.scene.add(wheel);
    }
  }

  private buildBattery(): void {
    const group = new THREE.Group();
    const materials: THREE.MeshStandardMaterial[] = [];
    const geometry = new THREE.BoxGeometry(0.26, 0.11, 0.42);
    for (let col = 0; col < MODULE_COLUMNS; col++) {
      for (let row = 0; row < MODULE_ROWS; row++) {
        const m = this.material();
        m.transparent = true;
        materials.push(m);
        const mesh = new THREE.Mesh(geometry, m);
        mesh.position.set(-0.95 + col * 0.29 - 0.03, 0.29, -0.46 + row * 0.46);
        group.add(mesh);
        this.modules.push(mesh);
      }
    }
    // Charge fills from the front of the pack, whichever order the modules were created in.
    this.modules.sort((a, b) => b.position.x - a.position.x || a.position.z - b.position.z);
    this.addPart("battery", group, new THREE.Vector3(0, 0.34, 0), materials);
  }

  private buildMotor(): void {
    const group = new THREE.Group();
    const m = this.material();
    const body = new THREE.Mesh(new THREE.CylinderGeometry(0.2, 0.2, 0.5, 20), m);
    body.rotation.x = Math.PI / 2;
    body.position.set(-1.4, WHEEL_RADIUS, 0);
    const axle = new THREE.Mesh(new THREE.CylinderGeometry(0.035, 0.035, 1.5, 8), new THREE.MeshStandardMaterial({ color: 0x777670, metalness: 0.6, roughness: 0.4 }));
    axle.rotation.x = Math.PI / 2;
    axle.position.copy(body.position);
    group.add(body, axle);
    this.addPart("motor", group, body.position.clone(), [m]);
  }

  private buildCooling(): void {
    const group = new THREE.Group();
    const m = this.material();
    const radiator = new THREE.Mesh(new THREE.BoxGeometry(0.07, 0.34, 1.1), m);
    radiator.position.set(2.02, 0.54, 0);
    group.add(radiator);
    for (const z of [-0.3, 0.3]) {
      const path = new THREE.CatmullRomCurve3([new THREE.Vector3(2.0, 0.42, z), new THREE.Vector3(1.55, 0.3, z), new THREE.Vector3(1.0, 0.3, z), new THREE.Vector3(0.95, 0.36, z)]);
      group.add(new THREE.Mesh(new THREE.TubeGeometry(path, 24, 0.028, 8, false), m));
    }
    this.addPart("cooling", group, radiator.position.clone(), [m]);
  }

  private buildChargePort(): void {
    const group = new THREE.Group();
    const m = this.material();
    const port = new THREE.Mesh(new THREE.CylinderGeometry(0.1, 0.1, 0.05, 20), m);
    port.rotation.x = Math.PI / 2;
    port.position.set(-1.75, 0.82, 0.93);
    group.add(port);

    const metal = new THREE.MeshStandardMaterial({ color: 0x4a4a46, roughness: 0.7 });
    this.chargePost = new THREE.Group();
    const post = new THREE.Mesh(new THREE.BoxGeometry(0.34, 1.3, 0.3), metal);
    post.position.set(-2.1, 0.65, 2.05);
    this.chargePost.add(post);
    this.chargePost.visible = false;
    this.scene.add(this.chargePost);

    this.chargeCurve = new THREE.CatmullRomCurve3([new THREE.Vector3(-2.1, 1.0, 1.9), new THREE.Vector3(-2.0, 0.5, 1.5), new THREE.Vector3(-1.85, 0.6, 1.15), port.position.clone()]);
    this.chargeCable = new THREE.Mesh(new THREE.TubeGeometry(this.chargeCurve, 32, 0.022, 8, false), m);
    this.chargeCable.visible = false;
    group.add(this.chargeCable);
    this.chargePulse = new THREE.Mesh(new THREE.SphereGeometry(0.05, 12, 12), new THREE.MeshBasicMaterial({ color: 0xffffff }));
    this.chargePulse.visible = false;
    this.scene.add(this.chargePulse);
    this.addPart("charge_port", group, port.position.clone(), [m]);
  }

  private buildAntenna(): void {
    const group = new THREE.Group();
    const m = this.material();
    const fin = new THREE.Mesh(new THREE.ConeGeometry(0.06, 0.2, 10), m);
    fin.position.set(-0.75, 1.6, 0);
    group.add(fin);
    this.addPart("antenna", group, fin.position.clone(), [m]);
  }

  private buildSensor(): void {
    const group = new THREE.Group();
    const m = this.material();
    const probe = new THREE.Mesh(new THREE.SphereGeometry(0.06, 14, 14), m);
    probe.position.set(0.25, 0.44, 0.5);
    const stem = new THREE.Mesh(new THREE.CylinderGeometry(0.012, 0.012, 0.1, 6), m);
    stem.position.set(0.25, 0.38, 0.5);
    group.add(probe, stem);
    this.addPart("sensor", group, probe.position.clone(), [m]);
  }

  private buildElectronics(): void {
    const group = new THREE.Group();
    const m = this.material();
    const box = new THREE.Mesh(new THREE.BoxGeometry(0.34, 0.16, 0.5), m);
    box.position.set(0.72, 0.68, 0);
    group.add(box);
    this.addPart("electronics", group, box.position.clone(), [m]);
  }
}
