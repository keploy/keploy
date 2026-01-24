// components/BookmarkButton.tsx
import { useState, useEffect } from 'react';
import { useBookmarks } from '@/hooks/useBookmarks';
import { motion, AnimatePresence } from 'framer-motion';

interface BookmarkButtonProps {
  slug: string;
  title?: string;
  excerpt?: string;
  size?: 'sm' | 'md' | 'lg';
  showLabel?: boolean;
  className?: string;
}

export default function BookmarkButton({
  slug,
  title,
  excerpt,
  size = 'md',
  showLabel = false,
  className = ''
}: BookmarkButtonProps) {
  const { isBookmarked, toggleBookmark } = useBookmarks();
  const [isAnimating, setIsAnimating] = useState(false);
  const [isBookmarkedState, setIsBookmarkedState] = useState(false);

  // Sync with hook state
  useEffect(() => {
    setIsBookmarkedState(isBookmarked(slug));
  }, [slug, isBookmarked]);

  const handleClick = () => {
    const newState = !isBookmarkedState;
    setIsBookmarkedState(newState);
    
    // Trigger animation
    setIsAnimating(true);
    setTimeout(() => setIsAnimating(false), 300);
    
    // Update bookmark
    toggleBookmark(slug, { title, excerpt });
  };

  const sizes = {
    sm: 'text-sm p-1',
    md: 'text-base p-2',
    lg: 'text-lg p-3'
  };

  return (
    <button
      onClick={handleClick}
      className={`flex items-center justify-center gap-2 rounded-full transition-all 
        ${isBookmarkedState 
          ? 'bg-primary-100 text-primary-600 hover:bg-primary-200' 
          : 'bg-gray-100 text-gray-600 hover:bg-gray-200'
        } 
        ${sizes[size]} ${className}`}
      aria-label={isBookmarkedState ? 'Remove from bookmarks' : 'Add to bookmarks'}
      title={isBookmarkedState ? 'Remove from reading list' : 'Save to reading list'}
    >
      <AnimatePresence mode="wait">
        {isAnimating ? (
          <motion.svg
            key="animating"
            initial={{ scale: 0, rotate: -180 }}
            animate={{ scale: 1, rotate: 0 }}
            exit={{ scale: 0, rotate: 180 }}
            className="w-5 h-5"
            fill="currentColor"
            viewBox="0 0 20 20"
          >
            <path d="M5 4a2 2 0 012-2h6a2 2 0 012 2v14l-5-2.5L5 18V4z" />
          </motion.svg>
        ) : (
          <motion.svg
            key="static"
            initial={{ scale: 1 }}
            animate={{ scale: isBookmarkedState ? 1.1 : 1 }}
            className="w-5 h-5"
            fill={isBookmarkedState ? "currentColor" : "none"}
            stroke="currentColor"
            strokeWidth="1.5"
            viewBox="0 0 24 24"
          >
            <path strokeLinecap="round" strokeLinejoin="round" 
              d="M17.593 3.322c1.1.128 1.907 1.077 1.907 2.185V21L12 17.25 4.5 21V5.507c0-1.108.806-2.057 1.907-2.185a48.507 48.507 0 0111.186 0z" 
            />
          </motion.svg>
        )}
      </AnimatePresence>
      
      {showLabel && (
        <span className="hidden sm:inline">
          {isBookmarkedState ? 'Saved' : 'Save'}
        </span>
      )}
    </button>
  );
}
