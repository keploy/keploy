// hooks/useBookmarks.ts
import { useState, useEffect, useCallback } from 'react';
import debounce from 'lodash/debounce';

const BOOKMARK_KEY = 'keploy-reading-list';
const SYNC_EVENT = 'bookmarks-updated';

interface Bookmark {
  slug: string;
  title?: string;
  excerpt?: string;
  bookmarkedAt: number;
  readStatus?: 'unread' | 'reading' | 'read';
}

interface UseBookmarksReturn {
  bookmarks: Map<string, Bookmark>;
  isBookmarked: (slug: string) => boolean;
  toggleBookmark: (slug: string, metadata?: Partial<Bookmark>) => void;
  addBookmark: (slug: string, metadata?: Partial<Bookmark>) => void;
  removeBookmark: (slug: string) => void;
  updateBookmark: (slug: string, updates: Partial<Bookmark>) => void;
  clearBookmarks: () => void;
  getBookmarksByStatus: (status?: Bookmark['readStatus']) => Bookmark[];
  count: number;
}

export function useBookmarks(): UseBookmarksReturn {
  const [bookmarks, setBookmarks] = useState<Map<string, Bookmark>>(new Map());

  // Load initial bookmarks
  useEffect(() => {
    if (typeof window === 'undefined') return;
    
    try {
      const saved = localStorage.getItem(BOOKMARK_KEY);
      if (saved) {
        const parsed = JSON.parse(saved);
        const map = new Map(Object.entries(parsed));
        setBookmarks(map);
      }
    } catch (error) {
      console.error('Failed to load bookmarks:', error);
      localStorage.removeItem(BOOKMARK_KEY);
    }
  }, []);

  // Optimized save with debouncing
  const saveBookmarks = useCallback(
    debounce((bookmarksMap: Map<string, Bookmark>) => {
      if (typeof window === 'undefined') return;
      
      try {
        const serializable = Object.fromEntries(bookmarksMap);
        localStorage.setItem(BOOKMARK_KEY, JSON.stringify(serializable));
        
        // Broadcast changes to other tabs
        window.dispatchEvent(new CustomEvent(SYNC_EVENT, {
          detail: { bookmarks: serializable }
        }));
      } catch (error) {
        console.error('Failed to save bookmarks:', error);
      }
    }, 500),
    []
  );

  // Sync across tabs
  useEffect(() => {
    const handleStorageSync = (event: CustomEvent) => {
      try {
        const map = new Map(Object.entries(event.detail.bookmarks));
        setBookmarks(map);
      } catch (error) {
        console.error('Failed to sync bookmarks:', error);
      }
    };

    const handleStorageChange = (e: StorageEvent) => {
      if (e.key === BOOKMARK_KEY && e.newValue) {
        try {
          const parsed = JSON.parse(e.newValue);
          const map = new Map(Object.entries(parsed));
          setBookmarks(map);
        } catch (error) {
          console.error('Failed to sync bookmarks:', error);
        }
      }
    };

    window.addEventListener(SYNC_EVENT as any, handleStorageSync);
    window.addEventListener('storage', handleStorageChange);

    return () => {
      window.removeEventListener(SYNC_EVENT as any, handleStorageSync);
      window.removeEventListener('storage', handleStorageChange);
    };
  }, []);

  const toggleBookmark = useCallback((slug: string, metadata?: Partial<Bookmark>) => {
    setBookmarks(prev => {
      const newMap = new Map(prev);
      
      if (newMap.has(slug)) {
        newMap.delete(slug);
      } else {
        newMap.set(slug, {
          slug,
          bookmarkedAt: Date.now(),
          readStatus: 'unread',
          ...metadata
        });
      }
      
      saveBookmarks(newMap);
      return newMap;
    });
  }, [saveBookmarks]);

  const addBookmark = useCallback((slug: string, metadata?: Partial<Bookmark>) => {
    setBookmarks(prev => {
      const newMap = new Map(prev);
      newMap.set(slug, {
        slug,
        bookmarkedAt: Date.now(),
        readStatus: 'unread',
        ...metadata
      });
      saveBookmarks(newMap);
      return newMap;
    });
  }, [saveBookmarks]);

  const removeBookmark = useCallback((slug: string) => {
    setBookmarks(prev => {
      const newMap = new Map(prev);
      newMap.delete(slug);
      saveBookmarks(newMap);
      return newMap;
    });
  }, [saveBookmarks]);

  const updateBookmark = useCallback((slug: string, updates: Partial<Bookmark>) => {
    setBookmarks(prev => {
      const newMap = new Map(prev);
      const existing = newMap.get(slug);
      if (existing) {
        newMap.set(slug, { ...existing, ...updates });
        saveBookmarks(newMap);
      }
      return newMap;
    });
  }, [saveBookmarks]);

  const clearBookmarks = useCallback(() => {
    setBookmarks(new Map());
    if (typeof window !== 'undefined') {
      localStorage.removeItem(BOOKMARK_KEY);
      window.dispatchEvent(new CustomEvent(SYNC_EVENT, {
        detail: { bookmarks: {} }
      }));
    }
  }, []);

  const getBookmarksByStatus = useCallback((status?: Bookmark['readStatus']) => {
    const bookmarksArray = Array.from(bookmarks.values());
    if (!status) return bookmarksArray;
    return bookmarksArray.filter(b => b.readStatus === status);
  }, [bookmarks]);

  const isBookmarked = useCallback((slug: string) => {
    return bookmarks.has(slug);
  }, [bookmarks]);

  return {
    bookmarks,
    isBookmarked,
    toggleBookmark,
    addBookmark,
    removeBookmark,
    updateBookmark,
    clearBookmarks,
    getBookmarksByStatus,
    count: bookmarks.size
  };
}
