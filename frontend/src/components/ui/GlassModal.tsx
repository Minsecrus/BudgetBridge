import { motion } from 'framer-motion'
import { X } from 'lucide-react'

// Glass overlay with spring enter/exit animation. The parent mounts/unmounts
// this component (BB's conditional modal pattern); wrap the conditional in
// <AnimatePresence> so the exit animation plays. backdropClose=false disables
// dismissal by clicking the backdrop (use for forms where accidental close
// would lose input).
export function GlassModal({ onClose, title, children, wide, backdropClose = true }: {
  onClose: () => void
  title: string
  children: React.ReactNode
  wide?: boolean
  backdropClose?: boolean
}) {
  return (
    <motion.div
      initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}
      onClick={e => { if (backdropClose && e.target === e.currentTarget) onClose() }}
      className="fixed inset-0 bg-black/60 backdrop-blur-sm flex items-center justify-center z-50 p-4">
      <motion.div
        initial={{ opacity: 0, y: 20, scale: 0.96 }}
        animate={{ opacity: 1, y: 0, scale: 1 }}
        exit={{ opacity: 0, y: 10, scale: 0.97 }}
        transition={{ type: 'spring', stiffness: 280, damping: 26 }}
        className={`glass rounded-2xl w-full ${wide ? 'max-w-lg' : 'max-w-md'} p-6 shadow-glass`}>
        <div className="flex items-center justify-between mb-5">
          <h2 className="font-semibold text-base text-gradient">{title}</h2>
          <button onClick={onClose} className="text-gray-400 hover:text-white transition-colors">
            <X className="w-4 h-4" />
          </button>
        </div>
        {children}
      </motion.div>
    </motion.div>
  )
}
