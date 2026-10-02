// Minimal FE-1 shell. Real pages arrive with FE-2 onward.

export default function App() {
  return (
    <div className="min-h-screen bg-neutral-950 text-neutral-100">
      <main className="mx-auto max-w-5xl px-6 py-16">
        <h1 className="text-2xl font-semibold tracking-tight">Lattice Console</h1>
        <p className="mt-2 text-neutral-400">
          Build pipeline is wired. Pages arrive in later phases.
        </p>
      </main>
    </div>
  )
}
